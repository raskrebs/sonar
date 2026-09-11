package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"reflect"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
)

// Watcher timing (step 5A.8).
const (
	// DefaultRefreshInterval is the least time between two routine
	// `docker ps` calls. Refreshes are driven by scans, so this only caps
	// how often scans ask; the daemon passes its base scan interval.
	DefaultRefreshInterval = 2 * time.Second
	// PSTimeout bounds one background `docker ps`. Healthy, it answers in
	// tens of milliseconds; nothing waits on it, so the timeout only decides
	// how soon a wedged Docker is noticed.
	PSTimeout = 5 * time.Second
	// MinBackoff is the wait after the first failed `docker ps`. Each further
	// failure doubles it, up to MaxBackoff; one success resets it.
	MinBackoff = 5 * time.Second
	// MaxBackoff caps the wait between attempts on a Docker that keeps
	// failing.
	MaxBackoff = 5 * time.Minute
	// statsWindow is how long after the last Stats call the watcher keeps
	// collecting container stats. Stats cost a second of sampling per
	// refresh, so they are only fetched while a scan wants them.
	statsWindow = 30 * time.Second
)

// health is what the watcher last learnt about Docker.
type health int

const (
	healthUnknown health = iota // no attempt has finished yet
	healthOK                    // the last `docker ps` answered
	healthDown                  // installed, but the last `docker ps` failed or timed out
	healthAbsent                // no docker on PATH
)

// WatcherOptions configures a Watcher. Every field may be zero.
type WatcherOptions struct {
	// Interval is the least time between routine refreshes. Zero means
	// DefaultRefreshInterval.
	Interval time.Duration
	// Logger receives the debug lines on health changes. Nil discards them.
	Logger *slog.Logger
	// OnChange is called after a refresh that changed the container list,
	// so the caller can scan again instead of waiting for its next tick.
	OnChange func()
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Watcher keeps the daemon's view of Docker off the scan path.
//
// A scan used to run `docker ps` inline, under the gate every `ports.kill`
// and `state.snapshot` queue behind, so a wedged Docker Desktop that never
// answered cost each scan the whole CLITimeout (step 5A.8). Now a scan only
// reads the list the watcher cached (Enrich, Stats) and pokes it; the watcher
// refreshes in its own goroutine, at most once per Interval, and backs off
// exponentially while Docker fails. A scan's container data is therefore at
// most one refresh old, and never waited for.
type Watcher struct {
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger
	onChange func()
	now      func() time.Time

	// Seams for tests; production runs the real CLI.
	lookPath func(string) (string, error)
	list     func(ctx context.Context) ([]container, error)
	stats    func([]container) map[string]*ContainerStats

	poke chan struct{}

	mu sync.Mutex
	// containers is the last good list. It is replaced, never modified, so
	// a reader may keep the slice after dropping the mutex.
	containers    []container
	statsCache    map[string]*ports.DockerStatsEntry
	statsWantedAt time.Time
	// seen is the port set of the last Enrich, for spotting new ports.
	seen map[int]bool
	// urgent asks for a refresh before Interval is up: a scan saw a port it
	// had not seen before and no container publishes it, or stats were just
	// asked for. Backoff still wins over it.
	urgent      bool
	lastAttempt time.Time
	retryAt     time.Time
	failures    int
	state       health
	lastErr     error
}

// NewWatcher builds a Watcher. It does nothing until Run.
func NewWatcher(o WatcherOptions) *Watcher {
	w := &Watcher{
		interval: o.Interval,
		timeout:  PSTimeout,
		logger:   o.Logger,
		onChange: o.OnChange,
		now:      o.Now,
		lookPath: exec.LookPath,
		list:     listContainersCtx,
		stats:    statsFor,
		poke:     make(chan struct{}, 1),
	}
	if w.interval <= 0 {
		w.interval = DefaultRefreshInterval
	}
	if w.logger == nil {
		w.logger = slog.New(slog.DiscardHandler)
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w
}

// Run refreshes whenever a scan pokes and a refresh is due, until ctx ends.
// The first refresh starts at once, so the list is primed before most first
// scans need it.
func (w *Watcher) Run(ctx context.Context) {
	for {
		if w.due() {
			w.refresh(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.poke:
		}
	}
}

// Poke asks for a refresh. It never blocks; the refresh happens only if one
// is due.
func (w *Watcher) Poke() {
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

// Enrich stamps container data from the cached list onto pp and pokes the
// watcher. It never runs a docker command.
func (w *Watcher) Enrich(pp []ports.ListeningPort) {
	w.mu.Lock()
	containers := w.containers
	published := publishedPorts(containers)
	seen := make(map[int]bool, len(pp))
	for i := range pp {
		port := pp[i].Port
		seen[port] = true
		if w.seen != nil && !w.seen[port] && !published[port] {
			// A port no container is known to publish: maybe a container
			// that started since the last refresh.
			w.urgent = true
		}
	}
	w.seen = seen
	w.mu.Unlock()

	enrichFrom(pp, containers)
	w.Poke()
}

// Stats returns the container stats cached by the last refresh, keyed by
// container name, and keeps the watcher collecting them for statsWindow. It
// never runs a docker command; nil means none are cached yet.
func (w *Watcher) Stats() map[string]*ports.DockerStatsEntry {
	w.mu.Lock()
	now := w.now()
	if w.statsWantedAt.IsZero() || now.Sub(w.statsWantedAt) >= statsWindow {
		w.urgent = true
	}
	w.statsWantedAt = now
	st := w.statsCache
	w.mu.Unlock()
	w.Poke()
	return st
}

// Healthy reports whether the last `docker ps` answered. Callers use it to
// skip optional docker calls, such as the container graph, while Docker is
// not answering.
func (w *Watcher) Healthy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state == healthOK
}

// due reports whether a refresh should run now.
func (w *Watcher) due() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if now.Before(w.retryAt) {
		return false
	}
	return w.urgent || w.lastAttempt.IsZero() || now.Sub(w.lastAttempt) >= w.interval
}

// refresh runs one `docker ps` (and the stats, when wanted) and records the
// outcome. It is only ever called from Run, so two never overlap.
func (w *Watcher) refresh(ctx context.Context) {
	w.mu.Lock()
	w.urgent = false // a request arriving during this refresh asks for another
	now := w.now()
	w.lastAttempt = now
	wantStats := !w.statsWantedAt.IsZero() && now.Sub(w.statsWantedAt) < statsWindow
	w.mu.Unlock()

	// Not installed is not a failure: no exec, no backoff, no noise. LookPath
	// is a few stat calls, so it is cheap to repeat, and a docker installed
	// later is picked up on the next refresh.
	if _, err := w.lookPath("docker"); err != nil {
		w.absent()
		return
	}

	lctx, cancel := context.WithTimeout(ctx, w.timeout)
	containers, err := w.list(lctx)
	timedOut := errors.Is(lctx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return // shutting down; this attempt says nothing about Docker
	}
	if err != nil {
		if timedOut {
			err = fmt.Errorf("`docker ps` did not answer within %s", w.timeout)
		}
		w.fail(err)
		return
	}

	var st map[string]*ports.DockerStatsEntry
	if wantStats {
		st = statsEntries(w.stats(containers))
	}
	w.succeed(containers, st)
}

// fail records a failed attempt: keep the last good list, stop serving stats
// and wait out the next backoff step.
func (w *Watcher) fail(err error) {
	w.mu.Lock()
	w.failures++
	wait := backoffFor(w.failures)
	w.retryAt = w.now().Add(wait)
	was := w.state
	w.state = healthDown
	w.lastErr = err
	w.statsCache = nil
	w.mu.Unlock()

	if was != healthDown {
		w.logger.Debug("docker is not answering; scans keep the last container list",
			"error", err, "retry_in", wait)
	}
}

// succeed records a good list and resets the backoff.
func (w *Watcher) succeed(containers []container, st map[string]*ports.DockerStatsEntry) {
	w.mu.Lock()
	was, failures := w.state, w.failures
	w.state = healthOK
	w.failures = 0
	w.retryAt = time.Time{}
	w.lastErr = nil
	changed := !reflect.DeepEqual(w.containers, containers)
	w.containers = containers
	w.statsCache = st
	w.mu.Unlock()

	if was == healthDown {
		w.logger.Debug("docker is answering again", "failed_attempts", failures)
	}
	if changed && w.onChange != nil {
		w.onChange()
	}
}

// absent records that there is no docker on PATH.
func (w *Watcher) absent() {
	w.mu.Lock()
	was := w.state
	changed := len(w.containers) > 0
	w.state = healthAbsent
	w.failures = 0
	w.retryAt = time.Time{}
	w.lastErr = nil
	w.containers = nil
	w.statsCache = nil
	w.mu.Unlock()

	if was != healthAbsent {
		w.logger.Debug("docker CLI not found; container enrichment is off")
	}
	if changed && w.onChange != nil {
		w.onChange()
	}
}

// backoffFor is the wait after the n-th consecutive failure: MinBackoff,
// doubling, capped at MaxBackoff.
func backoffFor(n int) time.Duration {
	d := MinBackoff
	for i := 1; i < n; i++ {
		d *= 2
		if d >= MaxBackoff {
			return MaxBackoff
		}
	}
	return d
}
