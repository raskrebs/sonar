// Package hoststats collects the load of the machine the daemon runs on: cpu,
// load average, memory, disk, uptime and kernel. The daemon publishes it as the
// `localhost` row of the snapshot's `hosts` collection, and step 3A.2 fills the
// rest of that collection with the same shape read from remote daemons.
//
// Every reading comes from the OS directly — /proc on Linux, sysctl on macOS,
// kernel32 on Windows — so the package builds and runs with CGO_ENABLED=0.
package hoststats

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/state"
)

// cpuSample is a cumulative CPU counter, read once per tick. A percentage is a
// property of an interval, never of an instant, so the collector keeps the
// previous sample and reports the work done between the two.
//
// Busy and Total are in whatever unit the platform counts in (jiffies on Linux,
// 100 ns on Windows, seconds on macOS); only their ratio is used. A platform
// that cannot count total — macOS, which has no cgo-free system-wide CPU
// counter — leaves Total zero and PerProc set, and the denominator becomes the
// wall time between the samples multiplied by the number of cores.
type cpuSample struct {
	ok    bool
	busy  float64
	total float64
	cpus  int

	// perProc is cumulative CPU seconds per pid. When it is set, busy is the
	// sum over the pids present in both samples plus the whole of any pid that
	// appeared in between — a process that exits must not take its lifetime's
	// CPU out of one interval's delta.
	perProc map[int]float64
}

// reading is one platform's answer for one tick. Every field is optional: a
// container with no /proc, a denied sysctl or a busy disk leaves a hole, and a
// hole is published as null rather than as a zero that reads like a fact.
type reading struct {
	kernel    string
	uptimeS   *int64
	load      []float64
	memUsed   *int64
	memTotal  *int64
	diskUsed  *int64
	diskTotal *int64
	diskPath  string
	cpu       cpuSample
}

// Collector reads the local machine's load. It is stateful because CPU percent
// is a delta: keep one across the daemon's lifetime and call Collect once per
// scan tick.
type Collector struct {
	now  func() time.Time
	read func(context.Context) (reading, error)
	// gpu is the GPU cache; nil collects no GPUs (GPUs stay null).
	gpu *gpuSampler

	mu       sync.Mutex
	prev     cpuSample
	prevAt   time.Time
	havePrev bool
}

// New builds a Collector reading this OS. Only the daemon builds one: the CLI
// reads host load from the daemon and never collects it in-process, so the GPU
// sampler's forks never land on a CLI command's path.
func New() *Collector {
	return &Collector{now: time.Now, read: readHost, gpu: newGPUSampler(probeGPU)}
}

// Collect returns the local machine as a state.Host. The identity fields the
// daemon owns — daemon and protocol version, the port and group counts — are
// left to the caller; everything the OS knows is filled in here.
//
// CPUPercent is null on the first call: one sample is not a measurement. GPUs
// is the GPU sampler's cached reading (see gpuSampler): Collect never waits on
// a GPU read, so it is null for the first few seconds of a daemon's life.
func (c *Collector) Collect(ctx context.Context) (state.Host, error) {
	var gpus []state.GPU
	if c.gpu != nil {
		gpus = c.gpu.Latest()
	}
	r, err := c.read(ctx)
	at := c.now()
	h := state.Host{
		Name:       state.LocalhostName,
		Address:    state.LocalhostName,
		Status:     state.HostConnected,
		LatencyMs:  0,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Kernel:     r.kernel,
		UptimeS:    r.uptimeS,
		Load:       r.load,
		MemoryUsed: r.memUsed, MemoryTotal: r.memTotal,
		DiskUsed: r.diskUsed, DiskTotal: r.diskTotal,
		DiskPath: r.diskPath,
		GPUs:     gpus,
		LastSeen: at.Format(time.RFC3339),
	}
	h.CPUPercent = c.cpuPercent(r.cpu, at)
	if err != nil {
		return h, err
	}
	return h, nil
}

// cpuPercent folds this tick's counter into the previous one and stores it.
func (c *Collector) cpuPercent(s cpuSample, at time.Time) *float64 {
	if !s.ok {
		return nil
	}
	c.mu.Lock()
	prev, prevAt, have := c.prev, c.prevAt, c.havePrev
	c.prev, c.prevAt, c.havePrev = s, at, true
	c.mu.Unlock()

	if !have {
		return nil
	}
	pct, ok := percentBetween(prev, prevAt, s, at)
	if !ok {
		return nil
	}
	return &pct
}

// percentBetween is the CPU maths, shared by all three platforms.
func percentBetween(prev cpuSample, prevAt time.Time, cur cpuSample, at time.Time) (float64, bool) {
	busy := cur.busy - prev.busy
	if cur.perProc != nil && prev.perProc != nil {
		busy = perProcDelta(prev.perProc, cur.perProc)
	}
	if busy < 0 {
		return 0, false
	}

	total := cur.total - prev.total
	if cur.total == 0 && prev.total == 0 {
		// No system-wide denominator: the interval's capacity is wall time
		// times the number of cores.
		cpus := cur.cpus
		if cpus < 1 {
			cpus = runtime.NumCPU()
		}
		total = at.Sub(prevAt).Seconds() * float64(cpus)
	}
	if total <= 0 {
		return 0, false
	}

	pct := busy / total * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// One decimal: a load meter that redraws on the fourth digit of a float
	// would publish a delta on every tick forever (see state.hostsEqual).
	return math.Round(pct*10) / 10, true
}

// perProcDelta sums the CPU time spent between two per-process samples: the
// growth of every process alive in both, plus the whole of every process that
// appeared in between. A process that exited contributes nothing — its last
// slice of work is lost, which understates the interval slightly and is far
// better than the negative total a plain sum would give.
func perProcDelta(prev, cur map[int]float64) float64 {
	var busy float64
	for pid, now := range cur {
		before, existed := prev[pid]
		switch {
		case !existed:
			busy += now
		case now > before:
			busy += now - before
		}
	}
	return busy
}

func i64(v int64) *int64  { return &v }
func u64(v uint64) *int64 { return i64(int64(v)) }
