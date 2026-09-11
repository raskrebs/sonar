package hoststats

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/state"
)

const (
	// GPUInterval is how often the GPUs are read while the daemon is being
	// asked for host load. It is slower than the 1 s stats tick on purpose: a
	// GPU source is a fork (ioreg, nvidia-smi), and a GPU meter that moves
	// every five seconds is plenty.
	GPUInterval = 5 * time.Second

	// GPUTimeout bounds one read. nvidia-smi can stall for seconds on a card
	// that is waking up or wedged; the read is killed rather than waited on.
	GPUTimeout = 2 * time.Second

	// gpuMaxBackoff caps how far a failing source is pushed out: a read that
	// keeps failing is retried at most every five minutes, not every tick.
	gpuMaxBackoff = 5 * time.Minute

	// gpuWaitDelay is how long a killed GPU command may take to release its
	// pipes before Wait gives up on it.
	gpuWaitDelay = 500 * time.Millisecond
)

// gpuReader reads every GPU once. It must honour ctx.
type gpuReader func(ctx context.Context) ([]state.GPU, error)

// gpuSampler keeps the latest GPU reading for Collect, which never waits on it.
//
// There is no long-lived goroutine. Collect asks for the cached value; when
// that value is older than the interval and no read is in flight, a single
// goroutine is started to refresh it under a hard timeout, and Collect returns
// the old value without waiting. The sampler therefore runs only while the
// daemon's own ticks run: a parked loop collects nothing and forks nothing.
//
// The platform probe runs once, inside the first refresh. A platform with no
// source (probe returns nil) is unsupported for the life of the process: the
// sampler never probes or spawns anything again, and GPUs stay null.
type gpuSampler struct {
	probe    func() gpuReader
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	// refreshed, when set, is called at the end of every refresh. Tests use it
	// to wait for the background goroutine.
	refreshed func()

	mu       sync.Mutex
	probed   bool
	read     gpuReader
	latest   []state.GPU
	nextAt   time.Time
	inflight bool
	failures int
}

func newGPUSampler(probe func() gpuReader) *gpuSampler {
	return &gpuSampler{probe: probe, interval: GPUInterval, timeout: GPUTimeout, now: time.Now}
}

// Latest returns the cached GPUs — nil until the first read lands, after a
// failed read, and forever on an unsupported platform — and starts a refresh
// in the background when one is due. It never blocks on a read.
//
// The returned slice is shared with the cache and must not be modified. The
// cache replaces it wholesale on every refresh and never writes into it.
func (s *gpuSampler) Latest() []state.GPU {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probed && s.read == nil {
		return nil
	}
	if !s.inflight && !s.now().Before(s.nextAt) {
		s.inflight = true
		go s.refresh()
	}
	return s.latest
}

// refresh probes (the first time) and reads once. Only one runs at a time:
// Latest sets inflight before starting it, and it clears inflight last.
func (s *gpuSampler) refresh() {
	defer func() {
		if s.refreshed != nil {
			s.refreshed()
		}
	}()

	s.mu.Lock()
	probed, read := s.probed, s.read
	s.mu.Unlock()
	if !probed {
		read = s.probe()
		s.mu.Lock()
		s.probed, s.read = true, read
		s.mu.Unlock()
	}
	if read == nil {
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	gpus, err := read(ctx)
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	delay := s.interval
	if err != nil || gpus == nil {
		// Not collected this time: null, and the source is left alone for
		// longer on each consecutive failure.
		s.latest = nil
		s.failures++
		for i := 1; i < s.failures && delay < gpuMaxBackoff; i++ {
			delay *= 2
		}
		if delay > gpuMaxBackoff {
			delay = gpuMaxBackoff
		}
	} else {
		s.latest = gpus
		s.failures = 0
	}
	s.nextAt = s.now().Add(delay)
	s.inflight = false
}

// combineGPUReaders reads several sources in turn (NVIDIA and AMD on one
// Linux box) and concatenates them. One source failing does not hide the
// others; only when every source fails is the read a failure.
func combineGPUReaders(readers ...gpuReader) gpuReader {
	if len(readers) == 1 {
		return readers[0]
	}
	return func(ctx context.Context) ([]state.GPU, error) {
		gpus := []state.GPU{}
		var errs []error
		for _, r := range readers {
			got, err := r(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			gpus = append(gpus, got...)
		}
		if len(errs) == len(readers) {
			return nil, errors.Join(errs...)
		}
		return gpus, nil
	}
}

// runGPUCommand runs one GPU tool under ctx. A timeout kills the process, and
// gpuWaitDelay stops a child that inherited its pipes from holding Wait open.
func runGPUCommand(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = gpuWaitDelay
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ioregGPUReader reads Apple's IOAccelerator registry entries.
func ioregGPUReader(path string) gpuReader {
	return func(ctx context.Context) ([]state.GPU, error) {
		out, err := runGPUCommand(ctx, path, "-r", "-d", "1", "-c", "IOAccelerator")
		if err != nil {
			return nil, err
		}
		return parseIoreg(out), nil
	}
}

// nvidiaGPUReader reads every NVIDIA GPU through nvidia-smi.
func nvidiaGPUReader(path string) gpuReader {
	return func(ctx context.Context) ([]state.GPU, error) {
		out, err := runGPUCommand(ctx, path, nvidiaSMIArgs...)
		if err != nil {
			return nil, err
		}
		return parseNvidiaSMI(out)
	}
}

// amdGPUReader reads the amdgpu counters sysfs exposes. Nothing is spawned.
func amdGPUReader(devs []string) gpuReader {
	return func(context.Context) ([]state.GPU, error) { return readAMD(devs), nil }
}

// noGPUReader is a source that has looked and found no GPU at all.
func noGPUReader(context.Context) ([]state.GPU, error) { return []state.GPU{}, nil }

// probeLinuxGPUs picks the Linux sources once: nvidia-smi when it is on PATH,
// sysfs for every amdgpu card. Intel GPUs are skipped: the kernel exposes no
// utilization counter for them without root and perf, and a row with no
// readings would only say that an iGPU exists.
//
// With no source, a readable drm class that lists no card at all is a machine
// with no GPU ([]); anything else — Intel only, NVIDIA without nvidia-smi, a
// virtual display adapter, no sysfs — is unsupported (null).
func probeLinuxGPUs(lookPath func(string) (string, error), sysRoot string) gpuReader {
	var readers []gpuReader
	if path, err := lookPath("nvidia-smi"); err == nil {
		readers = append(readers, nvidiaGPUReader(path))
	}
	if devs := amdDevices(sysRoot); len(devs) > 0 {
		readers = append(readers, amdGPUReader(devs))
	}
	if len(readers) > 0 {
		return combineGPUReaders(readers...)
	}
	if n, readable := drmCards(sysRoot); readable && n == 0 {
		return noGPUReader
	}
	return nil
}

// probeDarwinGPUs finds ioreg. It lives in /usr/sbin, which a stripped-down
// PATH may not carry, so the well-known path is the fallback.
func probeDarwinGPUs(lookPath func(string) (string, error), fallback string) gpuReader {
	if path, err := lookPath("ioreg"); err == nil {
		return ioregGPUReader(path)
	}
	if fallback != "" {
		if path, err := lookPath(fallback); err == nil {
			return ioregGPUReader(path)
		}
	}
	return nil
}
