package hoststats

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/state"
)

// testSampler is a gpuSampler over a scripted clock whose refreshes can be
// waited for.
type testSampler struct {
	*gpuSampler
	clockMu sync.Mutex
	at      time.Time
	done    chan struct{}
	probes  atomic.Int32
}

func newTestSampler(t *testing.T, read gpuReader) *testSampler {
	t.Helper()
	ts := &testSampler{at: clock, done: make(chan struct{}, 16)}
	ts.gpuSampler = &gpuSampler{
		probe: func() gpuReader {
			ts.probes.Add(1)
			return read
		},
		interval: GPUInterval,
		timeout:  GPUTimeout,
		now: func() time.Time {
			ts.clockMu.Lock()
			defer ts.clockMu.Unlock()
			return ts.at
		},
		refreshed: func() { ts.done <- struct{}{} },
	}
	return ts
}

func (ts *testSampler) advance(d time.Duration) {
	ts.clockMu.Lock()
	ts.at = ts.at.Add(d)
	ts.clockMu.Unlock()
}

// wait blocks until one background refresh has finished.
func (ts *testSampler) wait(t *testing.T) {
	t.Helper()
	select {
	case <-ts.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh finished within 5s")
	}
}

// noRefresh asserts that no refresh started from the last Latest call.
func (ts *testSampler) noRefresh(t *testing.T) {
	t.Helper()
	select {
	case <-ts.done:
		t.Fatal("a refresh ran when none was due")
	case <-time.After(20 * time.Millisecond):
	}
}

func oneGPU(util float64) []state.GPU {
	return []state.GPU{{Name: "Apple M5 Pro", UtilizationPercent: &util}}
}

// A platform with no source is probed once and then left alone for good: no
// second probe, no read, no spawn, however long the daemon runs.
func TestGPUSamplerUnsupportedIsNeverProbedAgain(t *testing.T) {
	ts := newTestSampler(t, nil)
	if got := ts.Latest(); got != nil {
		t.Fatalf("first Latest = %+v, want null", got)
	}
	ts.wait(t)
	for range 50 {
		ts.advance(time.Hour)
		if got := ts.Latest(); got != nil {
			t.Fatalf("Latest on an unsupported platform = %+v, want null", got)
		}
	}
	ts.noRefresh(t)
	if n := ts.probes.Load(); n != 1 {
		t.Fatalf("probed %d times, want exactly 1", n)
	}
}

// Latest reads the cache and returns at once, even while a read is stuck. The
// value lands on a later call, and the next read waits for the interval.
func TestGPUSamplerLatestNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	var reads atomic.Int32
	ts := newTestSampler(t, func(ctx context.Context) ([]state.GPU, error) {
		reads.Add(1)
		<-release
		return oneGPU(42), nil
	})

	start := time.Now()
	if got := ts.Latest(); got != nil {
		t.Fatalf("first Latest = %+v, want null before any read lands", got)
	}
	// The read is in flight and stuck; further calls neither wait nor start
	// a second read.
	for range 10 {
		ts.advance(time.Minute)
		if got := ts.Latest(); got != nil {
			t.Fatalf("Latest = %+v while the first read is in flight", got)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Latest took %v with a stuck read, want immediate", elapsed)
	}
	close(release)
	ts.wait(t)
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads started, want 1 (one in flight at a time)", n)
	}

	got := ts.Latest()
	if len(got) != 1 || *got[0].UtilizationPercent != 42 {
		t.Fatalf("Latest after the read = %+v, want the cached reading", got)
	}
	ts.noRefresh(t) // just refreshed: not due for another interval

	ts.advance(GPUInterval)
	ts.Latest()
	ts.wait(t)
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads after one interval, want 2", n)
	}
}

// A read that overruns is cancelled at the timeout, its value is null, and a
// failing source backs off rather than being retried every interval.
func TestGPUSamplerTimeoutAndBackoff(t *testing.T) {
	var sawDeadline atomic.Bool
	ts := newTestSampler(t, func(ctx context.Context) ([]state.GPU, error) {
		if _, ok := ctx.Deadline(); ok {
			sawDeadline.Store(true)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ts.timeout = 50 * time.Millisecond

	start := time.Now()
	ts.Latest()
	ts.wait(t)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a stuck read held the sampler for %v, want about the 50ms timeout", elapsed)
	}
	if !sawDeadline.Load() {
		t.Fatal("the read's context carried no deadline")
	}
	if got := ts.Latest(); got != nil {
		t.Fatalf("Latest after a timeout = %+v, want null", got)
	}

	// First failure: retried after one interval. Second: after two.
	ts.noRefresh(t)
	ts.advance(GPUInterval)
	ts.Latest()
	ts.wait(t)
	ts.advance(GPUInterval)
	ts.Latest()
	ts.noRefresh(t)
	ts.advance(GPUInterval)
	ts.Latest()
	ts.wait(t)
}

// A failed read clears the cache: the last good value is not passed off as a
// current one.
func TestGPUSamplerFailureClearsTheCache(t *testing.T) {
	fail := atomic.Bool{}
	ts := newTestSampler(t, func(context.Context) ([]state.GPU, error) {
		if fail.Load() {
			return nil, errors.New("nvidia-smi: exit status 9")
		}
		return oneGPU(10), nil
	})
	ts.Latest()
	ts.wait(t)
	if got := ts.Latest(); len(got) != 1 {
		t.Fatalf("Latest = %+v, want one GPU", got)
	}
	fail.Store(true)
	ts.advance(GPUInterval)
	ts.Latest()
	ts.wait(t)
	if got := ts.Latest(); got != nil {
		t.Fatalf("Latest after a failed read = %+v, want null", got)
	}
}

// Collect publishes the sampler's cache; a Collector with no sampler publishes
// null.
func TestCollectCarriesGPUs(t *testing.T) {
	c := fakeCollector(linuxReading(100, 1000))
	h, err := c.Collect(context.Background())
	if err != nil || h.GPUs != nil {
		t.Fatalf("no sampler: gpus = %+v, %v; want null", h.GPUs, err)
	}

	ts := newTestSampler(t, func(context.Context) ([]state.GPU, error) { return oneGPU(7), nil })
	c.gpu = ts.gpuSampler
	if h, _ := c.Collect(context.Background()); h.GPUs != nil {
		t.Fatalf("first Collect gpus = %+v, want null until the first read lands", h.GPUs)
	}
	ts.wait(t)
	h, _ = c.Collect(context.Background())
	if len(h.GPUs) != 1 || *h.GPUs[0].UtilizationPercent != 7 {
		t.Fatalf("Collect gpus = %+v, want the cached reading", h.GPUs)
	}
}

// combineGPUReaders keeps what one source read when another fails, and fails
// only when all do.
func TestCombineGPUReaders(t *testing.T) {
	ok := func(context.Context) ([]state.GPU, error) { return oneGPU(1), nil }
	bad := func(context.Context) ([]state.GPU, error) { return nil, errors.New("boom") }

	gpus, err := combineGPUReaders(ok, bad)(context.Background())
	if err != nil || len(gpus) != 1 {
		t.Fatalf("ok+bad = %+v, %v; want the ok source's GPU", gpus, err)
	}
	if _, err := combineGPUReaders(bad, bad)(context.Background()); err == nil {
		t.Fatal("bad+bad succeeded")
	}
}

// runGPUCommand kills a tool that outlives its context, and returns promptly.
func TestRunGPUCommandHonoursTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sleep binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runGPUCommand(ctx, "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("runGPUCommand took %v past a 100ms deadline", elapsed)
	}
}
