// Package scanner owns the daemon's single scan goroutine. It wraps the
// existing ports/docker enrichment pipeline, keeps the last good Snapshot, and
// publishes (previous, next) pairs so the daemon can compute whichever delta
// flavours its subscribers asked for.
package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/hoststats"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
)

// Timing constants from the spec's "Scanner loop" section.
const (
	// BaseInterval is the interval while anything is changing, and the
	// default `daemon.scan_interval` sets.
	BaseInterval = 2 * time.Second
	// MinScanInterval is the floor `daemon.scan_interval` may be set to. One
	// scan is an `lsof`/`ss`/`netstat` plus a docker round trip; below a
	// second a machine spends more of itself being measured than run.
	MinScanInterval = 1 * time.Second
	// MaxInterval caps the backoff on unchanged scans while the daemon is
	// serving RPC reads only. The constant is the ceiling at the default
	// base; a configured base scales it by IdleMaxFactor.
	MaxInterval = 10 * time.Second
	// SubscribedMaxInterval caps the backoff while at least one subscriber is
	// connected, so a live view never lags a change by more than this. Like
	// MaxInterval it is the ceiling at the default base, scaled by
	// SubscribedMaxFactor.
	SubscribedMaxInterval = 5 * time.Second
	// SubscribedMaxFactor and IdleMaxFactor scale the two ceilings with the
	// base interval, so `daemon.scan_interval` moves the whole adaptive curve
	// rather than only its floor. At the 2 s default they reproduce
	// SubscribedMaxInterval and MaxInterval exactly.
	SubscribedMaxFactor = 2.5
	IdleMaxFactor       = 5
	// BackoffFactor multiplies the interval after an unchanged scan.
	BackoffFactor = 1.5
	// CacheTTL is how long an RPC read may reuse the last scan.
	CacheTTL = 2 * time.Second
	// HealthCadence keeps HTTP probes off the port-scan cadence.
	HealthCadence = 10 * time.Second
	// HealthTimeout bounds a single probe.
	HealthTimeout = 2 * time.Second
	// HealthBudget bounds a whole round of opt-in health probes, however many
	// ports are listening. Ten probes run at once, so without a ceiling a
	// machine with forty listeners costs four waves of HealthTimeout — and a
	// write handler's republish is queued behind the scan that pays it
	// (contract §44).
	HealthBudget = 4 * time.Second
	// ConfiguredHealthBudget is the same ceiling for the `.sonar.yaml` health
	// paths, which are probed on every tick rather than on HealthCadence.
	ConfiguredHealthBudget = 2 * time.Second
	// ScanLockBudget bounds how long a handler waits for one of the loop's
	// gates before it gives up and answers from the last good snapshot. A scan is bounded
	// (the probes have budgets and the docker calls have timeouts), so
	// reaching this means the daemon is wedged; the point is that a reply is
	// still impossible to hang (contract §44).
	ScanLockBudget = 15 * time.Second
	// HostStatsTimeout bounds one collection of the machine's own load.
	HostStatsTimeout = 3 * time.Second
	// StatsInterval is the fixed cadence of the stats-only tick: per-process
	// cpu and memory plus this machine's load row, sampled without a port
	// scan behind it. Unlike the scan interval it never backs off, and it
	// only runs while someone is subscribed.
	StatsInterval = 1 * time.Second
	// MinStatsInterval is the floor `daemon.stats_interval` may be set to.
	// Below it the sampler costs more than the numbers it publishes are
	// worth: every tick is a `ps` per machine on Unix and a PowerShell on
	// Windows.
	MinStatsInterval = 250 * time.Millisecond
)

// Include is the per-subscriber opt-in from `state.subscribe {include}`.
type Include struct {
	Stats  bool
	Health bool
}

// Demand reports what the daemon's subscribers currently want. Returning zero
// subscribers stops the loop until Wake is called.
type Demand func() (subscribers int, include Include)

// Publisher receives every published change. prev is the snapshot the delta is
// computed against; next is the new state. Events are already derived.
type Publisher func(prev, next state.Snapshot, events []state.Event)

// Options configures a Loop. Only Scan is defaulted; the rest may be zero.
type Options struct {
	DaemonVersion string
	// ProtocolVersion is the wire version published on the localhost row of
	// the `hosts` collection. The daemon sets it; a bare Loop leaves it empty.
	ProtocolVersion string
	Logger          *slog.Logger
	Demand          Demand
	Publish         Publisher

	// Store persists renames, group pins, known `.sonar.yaml` roots and the
	// port history ring. Nil means the loop scans without a database.
	Store Store

	// Runs returns the run registry group attribution consults. It is a
	// function because `sonar start` installs the registry after the loop is
	// built; nil, or a nil return, means groups.PortRuns{}.
	Runs func() groups.Registry

	// Sessions builds the snapshot's `sessions` collection from the ports the
	// tick just resolved. It is a function for the same reason Runs is: the
	// daemon installs it from an OnStart hook, after the loop exists. Nil
	// publishes an empty collection.
	Sessions func(ports []state.Port) []state.SessionRecord

	// HostStats collects the machine's own load once per tick — cpu, load,
	// memory, disk, uptime. Tests inject a fake; production leaves it nil and
	// gets hoststats.Collect, whose CPU percent is a delta across the scan
	// interval and therefore needs the same collector every tick.
	HostStats func(ctx context.Context) (state.Host, error)

	// Remote returns the rows multiplexed in from the registered remote hosts,
	// already tagged with their host name (step 3A.2). It is a function
	// because the connection manager is installed from an OnStart hook, after
	// the loop exists; nil means "localhost only", which is what a daemon with
	// no registered hosts publishes.
	Remote func() state.Rows

	// Scan overrides the OS scan. Tests inject a fake; production leaves it nil
	// and gets ports.Scan + ports.Enrich, with container data from a
	// docker.Watcher's cached list rather than an inline `docker ps`
	// (step 5A.8).
	Scan func(include Include) ([]ports.ListeningPort, error)

	// Graph overrides the OS lookup of established connections between
	// listening ports. Tests inject a fake; production leaves it nil and gets
	// ports.BuildGraph + docker.BuildDockerGraph.
	//
	// It is a seam for the same reason Scan is. `ports.graph` and
	// `ports.inspect` answer from the snapshot for the listeners, but the
	// connections between them are a second, unrelated trip to the OS —
	// `netstat -ano` on Windows, `lsof` on macOS, `ss` on Linux, plus a
	// `docker inspect` whenever a container is listening. Left un-injectable,
	// a unit test of the *handler* pays for all of that on whatever machine CI
	// happens to be, and on a Windows runner with no Docker running it is
	// seconds, not milliseconds.
	Graph func(listening []ports.ListeningPort) ([]ports.Connection, error)

	// Probe overrides the health probe. Tests inject a fake; production leaves
	// it nil and gets ports.ProbeHealth. Both the configured-health tick and
	// the `ports.health` / `ports.inspect` handlers go through it, so a test
	// never opens a real socket.
	Probe Probe

	// SampleStats reads cpu, memory, state and uptime for pids the last
	// snapshot already named — the stats-only tick's one OS call. Tests
	// inject a fake; production leaves it nil and gets ports.SampleProcStats.
	SampleStats func(pids []int) map[int]ports.ProcSample

	// ScanInterval overrides the base port-scan cadence
	// (`daemon.scan_interval`): the interval used while something is changing
	// and the floor the backoff returns to. Zero means BaseInterval; anything
	// below MinScanInterval is clamped to it. Both backoff ceilings scale
	// with it, so the adaptive shape is the same whatever the base.
	ScanInterval time.Duration

	// StatsInterval overrides the stats-only tick's cadence
	// (`daemon.stats_interval`). Zero means StatsInterval; anything below
	// MinStatsInterval is clamped to it.
	StatsInterval time.Duration

	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Loop is the scan goroutine plus the cached snapshot RPC reads serve from.
type Loop struct {
	opts Options
	now  func() time.Time
	// base is the resolved `daemon.scan_interval`: the cadence while
	// something is changing and the floor the backoff returns to. It is fixed
	// at construction, so nothing locks to read it.
	base time.Duration

	wake chan struct{}
	// statsWake unparks the stats-only tick when a subscriber connects. It is
	// separate from wake so the two ticks can park and resume independently.
	statsWake chan struct{}

	attr attribution

	// docker keeps the container list the production scan enriches from,
	// refreshed in the background so no scan — and so no handler queued
	// behind one — ever waits on the docker CLI (step 5A.8). Nil when a test
	// injects its own Scan.
	docker *docker.Watcher

	// runGate admits one OS scan at a time. It is held from a scan's first
	// system call to its last, so two scans can never overlap and a slow one
	// can never commit port rows on top of a newer one's. Only scanning takes
	// it: a republish and a remote host's rows need no OS call at all.
	//
	// It is a buffered channel rather than a sync.Mutex because a handler that
	// waits for it has to wait *with a deadline* (contract §44).
	runGate chan struct{}

	// rpcGate does the same for the scans an RPC starts: one at a time, so ten
	// clients reading at once still cost the machine one scan. It is separate
	// from runGate on purpose. A `ports.kill` needs a bare port list and gets
	// it in a moment; the loop's own tick is collecting stats, which means
	// `docker stats` and a machine-wide `lsof`, and queueing the first behind
	// the second is what made a dry-run kill take double figures of seconds
	// (contract §44). The two can overlap because ordering no longer depends
	// on them not overlapping: attribution and the commit are serialized by
	// orderGate, and a scan whose OS half is older than the published snapshot
	// never commits (see commitScan).
	rpcGate chan struct{}

	// orderGate is what 1A.15's scanMu really guards: the attribution that
	// reads the store, the commit into the cache and the publish, taken by
	// everything that publishes a re-attributed snapshot — a scan, a store
	// write's republish, a remote host's rows changing.
	//
	// The rule it buys is unchanged (contract §38): a scan's attribution
	// happens either wholly before a store write or wholly after it, so the
	// tick that loaded the rename table before `ports.rename` wrote to it can
	// no longer commit on top of the write and put the old display_name back.
	// What changed in 1A.19 is where it starts. It used to be taken before the
	// OS scan, which meant a rename, a group pin or a "Save color" waited out
	// `lsof`, `ps`, `docker stats` and a round of health probes before its own
	// microsecond of work — seconds of latency for a guarantee that only ever
	// needed the store-reading half. Ordering is about attribution, not about
	// asking the kernel which sockets are open (contract §44).
	//
	// Lock order is runGate, then orderGate, then commitMu; never the reverse.
	orderGate chan struct{}

	// commitMu is the narrower half of that promise: it is held across the
	// commit into the cache *and* the publish that follows, by everything
	// that publishes — a scan, a remote host's rows changing, and the
	// stats-only tick. That is what makes delta seq order equal publish
	// order (contract §38), and it is all the stats tick needs.
	//
	// The tick deliberately does not take scanMu. A scan holds that from its
	// first OS call to its last, and with a container running `docker stats`
	// alone is two seconds of it — measured, on the machine this was built
	// for. A 1 s sampler queued behind that is not a 1 s sampler; it emitted
	// a burst of deltas every five or six seconds. Since the tick reads no
	// store and runs no scan, the write-ordering rule scanMu exists for
	// (1A.15) does not apply to it, and it holds commitMu across its own
	// read-sample-commit so it can still never publish a snapshot a scan has
	// already replaced.
	//
	// Lock order is scanMu then commitMu, never the reverse.
	commitMu sync.Mutex

	mu   sync.Mutex
	subs int
	snap state.Snapshot
	// local is the last scan's own rows, before the remote hosts were merged
	// in. RemoteChanged republishes from it, so a remote host's delta costs
	// nothing on the local machine: no OS scan, no attribution, no probes.
	local state.Rows
	// lastPorts is the OS half of the last scan, before attribution. Republish
	// re-attributes it instead of scanning the machine again, which is what
	// makes a rename or a `.sonar.yaml` write cost microseconds rather than a
	// full scan (contract §44).
	lastPorts []ports.ListeningPort
	haveSnap  bool

	// scanSeq numbers scans in the order their OS half begins, and
	// committedSeq is the number of the scan the published snapshot came
	// from. Both are counters rather than timestamps on purpose: "did a scan
	// begin after I asked" and "is this scan older than the one already
	// published" are happens-before questions, and a clock cannot answer
	// them. `time.Now` on Windows moves in steps of up to about 15 ms, so two
	// events milliseconds apart read as simultaneous — which made a kill's
	// rescan coalesce onto the scan that had *just* finished and resolve its
	// targets against the very snapshot §17 says it must not use.
	scanSeq      uint64
	committedSeq uint64

	lastScanAt   time.Time
	lastHealthAt time.Time
	interval     time.Duration
	seq          uint64
	scans        int64
	lastErr      error
}

// New builds a Loop. It does not start scanning; call Run.
func New(opts Options) *Loop {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Demand == nil {
		opts.Demand = func() (int, Include) { return 0, Include{} }
	}
	if opts.Publish == nil {
		opts.Publish = func(state.Snapshot, state.Snapshot, []state.Event) {}
	}
	if opts.Probe == nil {
		opts.Probe = ports.ProbeHealth
	}
	if opts.HostStats == nil {
		opts.HostStats = hoststats.New().Collect
	}
	if opts.SampleStats == nil {
		opts.SampleStats = ports.SampleProcStats
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	base := resolveScanInterval(opts.ScanInterval)
	l := &Loop{
		opts:      opts,
		now:       now,
		base:      base,
		wake:      make(chan struct{}, 1),
		statsWake: make(chan struct{}, 1),
		runGate:   make(chan struct{}, 1),
		rpcGate:   make(chan struct{}, 1),
		orderGate: make(chan struct{}, 1),
		interval:  base,
	}
	if l.opts.Scan == nil {
		// The refresh cadence is the scan cadence: scans poke the watcher,
		// and it asks Docker at most once per base interval. A refresh that
		// changes the list wakes the loop, so a new container's port is
		// enriched within one tick rather than one backed-off interval.
		l.docker = docker.NewWatcher(docker.WatcherOptions{
			Interval: base,
			Logger:   opts.Logger,
			OnChange: l.Wake,
		})
		l.opts.Scan = l.osScan
	}
	if l.opts.Graph == nil {
		l.opts.Graph = l.osGraph
	}
	return l
}

// resolveScanInterval clamps a configured base scan interval. Zero (the unset
// case) means BaseInterval; there is no "off", because the loop already parks
// itself whenever nothing is subscribed.
func resolveScanInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return BaseInterval
	}
	if d < MinScanInterval {
		return MinScanInterval
	}
	return d
}

// lock takes one of the loop's channel mutexes and waits as long as it takes.
// The scan loop itself uses this: it has nobody to answer to.
func lock(gate chan struct{}) { gate <- struct{}{} }

// lockFor takes one, giving up after d, and reports whether it got it. Every
// handler-initiated path uses this rather than lock, so a reply can never wait
// on a gate forever (contract §44).
func lockFor(gate chan struct{}, d time.Duration) bool {
	select {
	case gate <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case gate <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// unlock releases one.
func unlock(gate chan struct{}) { <-gate }

// osScan is the production scan: the same pipeline `sonar list` runs, so the
// daemon and the no-daemon path emit the same rows — except that container
// data and container stats come from the watcher's cache, at most one refresh
// old, instead of from a `docker ps` this scan would wait on. A wedged Docker
// used to cost every scan the whole docker.CLITimeout, and every `ports.kill`
// and `state.snapshot` queued behind it (step 5A.8).
func (l *Loop) osScan(include Include) ([]ports.ListeningPort, error) {
	pp, err := ports.Scan()
	if err != nil {
		return nil, err
	}
	l.docker.Enrich(pp)
	ports.Enrich(pp)
	if include.Stats {
		ports.EnrichStats(pp, l.docker.Stats())
	}
	return pp, nil
}

// osGraph is the production connection graph: the established links between
// listening ports, plus the ones Docker only knows about. The container half
// is skipped while the watcher knows Docker is not answering: it is a
// `docker inspect` and a `docker exec` per container, each of which would
// wait out the CLI timeout.
func (l *Loop) osGraph(listening []ports.ListeningPort) ([]ports.Connection, error) {
	edges, err := ports.BuildGraph(listening)
	if err != nil {
		return nil, err
	}
	if l.docker != nil && !l.docker.Healthy() {
		return edges, nil
	}
	containerEdges, err := docker.BuildDockerGraph(listening)
	if err != nil {
		return nil, fmt.Errorf("container graph: %w", err)
	}
	return append(edges, containerEdges...), nil
}

// Graph reports the established connections between the given listening ports.
// Callers pass the listeners they already have — from the snapshot — so this
// never re-scans for them.
func (l *Loop) Graph(listening []ports.ListeningPort) ([]ports.Connection, error) {
	return l.opts.Graph(listening)
}

// Probe runs one health probe through the loop's seam.
func (l *Loop) Probe(host string, port int, path string, timeout time.Duration) ports.HealthResult {
	return l.opts.Probe(host, port, path, timeout)
}

// SetDemand installs the demand callback after construction. The daemon uses
// it because the server and the loop refer to each other.
func (l *Loop) SetDemand(d Demand) {
	if d != nil {
		l.opts.Demand = d
	}
}

// SetPublisher installs the publish callback after construction.
func (l *Loop) SetPublisher(p Publisher) {
	if p != nil {
		l.opts.Publish = p
	}
}

// SetRemote installs the remote-rows provider after construction. The remote
// connection manager calls it from its OnStart hook.
func (l *Loop) SetRemote(f func() state.Rows) {
	if f != nil {
		l.opts.Remote = f
	}
}

// remoteRows is the current remote contribution, or nothing.
func (l *Loop) remoteRows() state.Rows {
	if l.opts.Remote == nil {
		return state.Rows{}
	}
	return l.opts.Remote()
}

// Cached returns the last published snapshot without scanning, as this machine
// alone: no remote host's rows.
//
// Local-only is the default on purpose. Everything inside the daemon that
// resolves a selector — `ports.kill`, `ports.inspect`, `groups.start`, the
// session handlers — reads a snapshot, and a port 3000 that also exists on a
// registered host must not make those calls ambiguous for a caller that never
// mentioned a host. A `host` on selectors is step 3A.4's job; until then, and
// after it for every caller that omits one, a snapshot means localhost.
//
// The two places that publish state to clients — state.subscribe's opening
// reply and the state.snapshot method — ask for CachedAll / SnapshotAll and
// apply the subscriber's own `hosts` filter.
func (l *Loop) Cached() state.Snapshot { return localOnly(l.CachedAll()) }

// CachedAll is Cached including every registered remote host's rows. It is
// what state.subscribe replies with, so a subscriber that asked for other
// hosts sees them in its opening snapshot rather than waiting for one of them
// to change.
func (l *Loop) CachedAll() state.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.haveSnap {
		// Even before the first scan `hosts` names this machine: a client
		// that subscribes at startup must not have to special-case an empty
		// collection to find localhost. The registered remote hosts are
		// already there too, so a subscriber never sees them blink in.
		base := state.Rows{Hosts: []state.Host{l.identityRow()}}
		return base.Append(l.remoteRows()).Normalize().Into(state.Snapshot{
			At:            l.now().Format(time.RFC3339),
			DaemonVersion: l.opts.DaemonVersion,
		})
	}
	return l.snap
}

// localOnly drops the remote hosts' rows. It is a no-op, allocation included,
// when there are none, which is every daemon with no host registered.
func localOnly(s state.Snapshot) state.Snapshot {
	for i := range s.Ports {
		if !state.IsLocalhost(s.Ports[i].Host) {
			return state.FilterSnapshot(s, state.LocalOnly())
		}
	}
	for i := range s.Hosts {
		if !state.IsLocalhost(s.Hosts[i].Name) {
			return state.FilterSnapshot(s, state.LocalOnly())
		}
	}
	for i := range s.Groups {
		if !state.IsLocalhost(s.Groups[i].Host) {
			return state.FilterSnapshot(s, state.LocalOnly())
		}
	}
	for i := range s.Sessions {
		if !state.IsLocalhost(s.Sessions[i].Host) {
			return state.FilterSnapshot(s, state.LocalOnly())
		}
	}
	return s
}

// Wake nudges a stopped or backed-off loop to scan now. Called when a
// subscriber connects or when an RPC reads state.
func (l *Loop) Wake() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
	select {
	case l.statsWake <- struct{}{}:
	default:
	}
}

// Run drives the loop until ctx is cancelled. With zero subscribers it parks on
// Wake and does no work at all; the next RPC or subscription starts it again.
//
// It also runs the stats-only tick (see runStats) in a second goroutine, and
// returns only once both have stopped. The two are serialized against each
// other by scanMu, never by being the same goroutine: the whole point is that
// a 1 s load sample does not have to wait for — or reset — an adaptive port
// scan that may be 5 s apart.
func (l *Loop) Run(ctx context.Context) {
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		l.runStats(ctx)
	}()
	if l.docker != nil {
		// The docker watcher refreshes the container list the scans read,
		// in its own goroutine for the same reason: nothing on the scan
		// path waits on the docker CLI.
		background.Add(1)
		go func() {
			defer background.Done()
			l.docker.Run(ctx)
		}()
	}
	defer background.Wait()

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		subs, include := l.opts.Demand()
		if subs == 0 {
			// Parked: no scanning, no timer.
			select {
			case <-ctx.Done():
				return
			case <-l.wake:
				continue
			}
		}

		// A scan an RPC ran a moment ago is this tick's scan. Wake means "the
		// state you are holding may be stale", and a scan younger than the
		// current interval is not: without this floor every `ports.kill`,
		// every `sonar list` that missed the cache and every write woke the
		// loop into a second full scan, which is one more scan for the next
		// caller to be queued behind (contract §44).
		//
		// The floor is the *loop's* cadence and nothing else. A handler that
		// asks for a scan gets one: Rescan is a forced scan (see scanMode)
		// and never consults this, because a kill resolving its selectors
		// against the previous tick is the bug §17 exists to prevent.
		wait := l.dueIn()
		if wait <= 0 {
			l.scanAndPublish(include)
			wait = l.dueIn()
		}
		if wait <= 0 {
			wait = l.base
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-l.wake:
			// A read or a new subscriber: back to the base interval.
			l.mu.Lock()
			l.interval = l.base
			l.mu.Unlock()
		}
	}
}

// scanAndPublish runs one scan, updates the cache, adapts the interval and
// publishes when something changed. One scan at a time (see scanMu); the
// publish itself happens inside scanLocked, under commitMu.
func (l *Loop) scanAndPublish(include Include) {
	lock(l.runGate)
	defer unlock(l.runGate)

	_, prev, _, err := l.scanLocked(include, Include{})
	if err != nil {
		l.opts.Logger.Warn("scan failed, keeping last good state", "error", err)
		// A scan_error carries no snapshot and takes no seq, so it needs no
		// place in the commit order.
		l.opts.Publish(prev, prev, []state.Event{{
			Kind: "scan_error",
			At:   l.nowRFC3339(),
			Data: map[string]any{"error": err.Error()},
		}})
	}
}

// publish hands the transition to the daemon and then writes its port
// transitions to the history ring, in that order: a client sees the change
// before the disk does (daemon spec, "SQLite").
func (l *Loop) publish(prev, next state.Snapshot, events []state.Event) {
	l.opts.Publish(prev, next, events)
	l.record(events)
}

// scanLocked performs a scan and swaps it into the cache. It returns the new
// snapshot, the one it replaced, and whether anything a client cares about
// changed.
//
// collect is what this scan asks the OS for. carry is what the *subscribers*
// want the published snapshot to keep but this caller has no reason to pay
// for: those fields are copied forward from the previous snapshot instead of
// being collected again (contract §44). The scan loop's own tick passes its
// demand as collect and nothing as carry; an RPC passes the caller's own
// include as collect and the subscribers' as carry.
func (l *Loop) scanLocked(collect, carry Include) (next, prev state.Snapshot, changed bool, err error) {
	// Read outside the mutex: Demand takes the server's own lock, which the
	// server holds while calling back into the loop.
	subs, _ := l.opts.Demand()
	wantHealth := collect.Health && l.healthDue()

	gen := l.beginScan()
	pp, err := l.opts.Scan(collect)
	if err != nil {
		l.mu.Lock()
		l.lastErr = err
		l.lastScanAt = l.now()
		l.scans++
		prev = l.snap
		l.mu.Unlock()
		return state.Snapshot{}, prev, false, err
	}

	if wantHealth {
		ports.EnrichHealth(pp, HealthTimeout, HealthBudget)
	}

	// The machine's own load, read before the ordering gate: on macOS it forks
	// `ps`, and nothing about it is ordered against a store write.
	host := l.collectHost()

	// Everything from here on is the ordered half of the scan: the attribution
	// that reads the store, the commit, and the publish. It is serialized
	// against every store write's republish and against every other publisher,
	// and the publish rides inside it so a later seq can never reach a client
	// first (contract §38, §44).
	lock(l.orderGate)
	defer unlock(l.orderGate)

	// Resolve every port's group with the pins the store holds, apply the
	// stored renames and build the group collection, all before the snapshot
	// is assembled: what gets published is already attributed and named.
	rows, groupRows := l.attribute(pp)

	sessionRows := l.sessions(rows)

	// Configured health comes after attribution because it is the groups that
	// say which port has a `health:` path. It runs on every tick regardless of
	// `include`: a health path in a `.sonar.yaml` is part of what the service
	// is, not an opt-in statistic (step 1A.7). Its budget is what keeps this
	// short enough to sit inside the ordering gate.
	probeConfigured(rows, groupRows, l.opts.Probe, ConfiguredHealthBudget)

	l.commitMu.Lock()
	defer l.commitMu.Unlock()

	next, prev, changed = l.commitScan(commit{
		subs:     subs,
		collect:  collect,
		carry:    carry,
		health:   wantHealth,
		seq:      gen,
		pp:       pp,
		rows:     rows,
		groups:   groupRows,
		sessions: sessionRows,
		host:     host,
	})
	if changed {
		l.publish(prev, next, deriveEvents(prev, next, l.nowRFC3339()))
	}
	return next, prev, changed, nil
}

// commit is one finished scan on its way into the cache.
type commit struct {
	subs    int
	collect Include
	carry   Include
	health  bool
	// seq is the number this scan took when its OS half began.
	seq      uint64
	pp       []ports.ListeningPort
	rows     []state.Port
	groups   []state.Group
	sessions []state.SessionRecord
	host     state.Host
}

// commitScan swaps a finished scan into the cache and adapts the interval.
// Caller holds the ordering gate and commitMu; this takes l.mu for the swap
// itself.
func (l *Loop) commitScan(c commit) (next, prev state.Snapshot, changed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.subs = c.subs
	prev = l.snap
	rows, groupRows := c.rows, c.groups

	l.scans++
	l.lastErr = nil
	if l.haveSnap && c.seq < l.committedSeq {
		// Superseded. This scan asked the OS what was listening before the
		// snapshot on the wire was captured, so committing it would put back
		// ports a newer scan has already seen go — the "no going backwards"
		// half of contract §38, now enforced at the commit rather than by
		// forbidding two scans to overlap at all (contract §44).
		return prev, prev, false
	}

	// Carry forward every probe result this scan did not take: between health
	// cadences, for a configured probe the round's budget cut short, and for a
	// scan an RPC started that had no reason to pay for a subscriber's health
	// (contract §44). Without it a port flickers between "checked" and
	// "unknown" on the wire.
	carryHealth(prev.Ports, rows, (c.collect.Health || c.carry.Health) && !c.health)

	// Same for stats. A scan an RPC started collects none unless the caller
	// asked, so it copies the subscribers' last readings across rather than
	// publishing a snapshot with the numbers stripped out — and the 1 s stats
	// tick refreshes them within the second anyway (contract §42).
	if c.carry.Stats && !c.collect.Stats {
		carryStats(prev.Ports, rows)
	}
	withStats := c.collect.Stats || c.carry.Stats

	l.lastScanAt = l.now()
	l.committedSeq = c.seq
	l.lastPorts = c.pp
	if c.health {
		l.lastHealthAt = l.lastScanAt
	}

	host := c.host
	host.Ports, host.Groups = len(rows), len(groupRows)
	host.LastSeen = l.lastScanAt.Format(time.RFC3339)

	// Tunnels and proxies belong to spec 3. Every collection is always an
	// array, never null.
	local := state.Rows{
		Ports:    rows,
		Groups:   groupRows,
		Sessions: c.sessions,
		Hosts:    []state.Host{host},
	}.Tag(state.LocalhostName).Normalize()
	l.local = local

	next = local.Append(l.remoteRows()).Into(state.Snapshot{
		At:            l.lastScanAt.Format(time.RFC3339),
		DaemonVersion: l.opts.DaemonVersion,
	})

	if l.haveSnap && !snapshotChanged(prev, next, withStats) {
		if !hostChanged(prev, next) {
			// Nothing to publish: keep the previous seq and back the interval
			// off.
			next.Seq = prev.Seq
			l.snap = next
			l.interval = backoff(l.interval, l.base, l.maxIntervalLocked())
			return next, prev, false
		}
		// Only the machine's own load moved. It is published — host load is
		// state, not an opt-in statistic, so it reaches every subscriber
		// whatever their `include` (contract §22's rule for configured
		// health) — but it does not snap the interval back to the base. CPU
		// percent moves on almost every tick, and letting it reset the
		// backoff would pin a subscribed daemon to a full port scan every two
		// seconds forever.
		l.seq++
		next.Seq = l.seq
		l.snap = next
		l.interval = backoff(l.interval, l.base, l.maxIntervalLocked())
		return next, prev, true
	}

	l.seq++
	next.Seq = l.seq
	l.snap = next
	l.haveSnap = true
	l.interval = l.base
	return next, prev, true
}

// RemoteChanged republishes the current state with the remote hosts' rows as
// they are now. It runs no OS scan: the local half of the last tick is reused
// verbatim, so a remote daemon publishing a delta every two seconds costs the
// local machine a diff and a marshal rather than a full port scan.
//
// It takes scanMu like a scan does, so its publish cannot interleave with one
// and delta seq order stays publish order (contract §38).
//
// Before the first local scan it publishes against the identity-only snapshot
// CachedAll synthesises rather than declining to publish. Waking the loop and
// leaving the rows for the first tick looked equivalent and was not: a bridge
// that connects in that window reaches no subscriber until a scan lands, and
// nothing tells the subscriber a host appeared. It still wakes the loop, so
// the local half follows.
func (l *Loop) RemoteChanged() {
	lock(l.orderGate)
	defer unlock(l.orderGate)
	l.commitMu.Lock()
	defer l.commitMu.Unlock()

	l.mu.Lock()
	if !l.haveSnap {
		// The same base CachedAll serves before the first scan, so what a
		// subscriber is told and what a new subscriber reads agree.
		l.local = state.Rows{Hosts: []state.Host{l.identityRow()}}.Normalize()
		l.snap = l.local.Into(state.Snapshot{
			At:            l.now().Format(time.RFC3339),
			DaemonVersion: l.opts.DaemonVersion,
		})
		l.haveSnap = true
		defer l.Wake()
	}
	prev := l.snap
	next := l.local.Append(l.remoteRows()).Into(state.Snapshot{
		At:              l.now().Format(time.RFC3339),
		DaemonVersion:   l.opts.DaemonVersion,
		ExposuresActive: prev.ExposuresActive,
	})
	if !snapshotChanged(prev, next, true) && !hostChanged(prev, next) {
		l.mu.Unlock()
		return
	}
	l.seq++
	next.Seq = l.seq
	l.snap = next
	l.mu.Unlock()

	l.publish(prev, next, nil)
}

// collectHost reads this machine's load for the tick. A failure is not a scan
// failure: the row is published anyway, with the reason attached and every
// load field null, because `hosts` always names localhost.
func (l *Loop) collectHost() state.Host {
	ctx, cancel := context.WithTimeout(context.Background(), HostStatsTimeout)
	defer cancel()

	h, err := l.opts.HostStats(ctx)
	if err != nil {
		l.opts.Logger.Warn("host stats failed", "error", err)
		reason := err.Error()
		h.StatusReason = &reason
	}
	h.Name, h.Address, h.Status = state.LocalhostName, state.LocalhostName, state.HostConnected
	h.DaemonVersion, h.ProtocolVersion = l.opts.DaemonVersion, l.opts.ProtocolVersion
	return h
}

// identityRow is the localhost row before any load has been collected: who
// this daemon is, with every measurement still null.
func (l *Loop) identityRow() state.Host {
	return state.Host{
		Name:            state.LocalhostName,
		Address:         state.LocalhostName,
		Status:          state.HostConnected,
		DaemonVersion:   l.opts.DaemonVersion,
		ProtocolVersion: l.opts.ProtocolVersion,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		LastSeen:        l.now().Format(time.RFC3339),
	}
}

// hostChanged reports whether the `hosts` collection moved between two
// snapshots.
func hostChanged(prev, next state.Snapshot) bool {
	d := state.DiffHosts(prev.Hosts, next.Hosts)
	return len(d.Added) > 0 || len(d.Updated) > 0 || len(d.Removed) > 0
}

// dueIn is how long until the next scan is due: the current interval minus the
// age of the last scan, whoever ran it. Zero or less means now.
func (l *Loop) dueIn() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastScanAt.IsZero() {
		return 0
	}
	return l.interval - l.now().Sub(l.lastScanAt)
}

// healthDue reports whether the health cadence has elapsed.
func (l *Loop) healthDue() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastHealthAt.IsZero() || l.now().Sub(l.lastHealthAt) >= HealthCadence
}

// maxIntervalLocked is the ceiling the backoff may reach right now: 2.5x the
// base while anyone is subscribed, so a live view never lags a change by more
// than that, and 5x when the daemon only answers RPC reads. At the default 2 s
// base those are the 5 s and 10 s of the spec. Caller holds the mutex.
func (l *Loop) maxIntervalLocked() time.Duration {
	factor := float64(IdleMaxFactor)
	if l.subs > 0 {
		factor = SubscribedMaxFactor
	}
	return time.Duration(float64(l.base) * factor)
}

// backoff multiplies the interval by 1.5, floored at base and capped at max.
func backoff(d, base, max time.Duration) time.Duration {
	next := time.Duration(float64(d) * BackoffFactor)
	if next > max {
		return max
	}
	if next < base {
		return base
	}
	return next
}

// snapshotChanged reports whether the diff between two snapshots is non-empty.
func snapshotChanged(prev, next state.Snapshot, withStats bool) bool {
	d := state.Diff(prev, next)
	if withStats {
		d = state.DiffWithStats(prev, next)
	}
	return len(d.Ports.Added) > 0 || len(d.Ports.Updated) > 0 || len(d.Ports.Removed) > 0 ||
		len(d.Groups.Added) > 0 || len(d.Groups.Updated) > 0 || len(d.Groups.Removed) > 0 ||
		len(d.Sessions.Added) > 0 || len(d.Sessions.Updated) > 0 || len(d.Sessions.Removed) > 0
}

// carryHealth copies health results from the previous snapshot onto rows that
// were not probed this tick.
//
// all says whether to carry every result or only the configured ones. A
// `.sonar.yaml` health path is state rather than an opt-in statistic
// (contract §22), so its last verdict is always better than the "unknown" a
// skipped probe would publish; an opt-in probe is only carried while somebody
// is still asking for health.
func carryHealth(prev []state.Port, next []state.Port, all bool) {
	if len(prev) == 0 {
		return
	}
	byKey := make(map[string]*state.Health, len(prev))
	for i := range prev {
		byKey[prev[i].Key()] = prev[i].Health
	}
	for i := range next {
		if next[i].Health != nil {
			continue
		}
		h, ok := byKey[next[i].Key()]
		if !ok || h == nil {
			continue
		}
		if all || h.Configured {
			next[i].Health = h
		}
	}
}

// carryStats copies the previous snapshot's stats onto rows that were not
// sampled this scan, keyed the same way health is. A port that appeared in
// this scan and has no previous reading keeps a null `stats`; the 1 s stats
// tick fills it in on its next pass.
func carryStats(prev []state.Port, next []state.Port) {
	if len(prev) == 0 {
		return
	}
	byKey := make(map[string]*state.Stats, len(prev))
	for i := range prev {
		byKey[prev[i].Key()] = prev[i].Stats
	}
	for i := range next {
		if next[i].Stats != nil {
			continue
		}
		if st, ok := byKey[next[i].Key()]; ok {
			next[i].Stats = st
		}
	}
}

// deriveEvents turns a snapshot transition into the discrete events the tray
// and app show as notifications.
func deriveEvents(prev, next state.Snapshot, at string) []state.Event {
	d := state.DiffWithStats(prev, next)

	// A key that is both removed and added in one delta is a restart.
	removed := make(map[string]bool, len(d.Ports.Removed))
	for _, k := range d.Ports.Removed {
		removed[k] = true
	}

	events := make([]state.Event, 0, len(d.Ports.Added)+len(d.Ports.Removed))
	restarted := make(map[string]bool, len(d.Ports.Added))
	for i := range d.Ports.Added {
		p := d.Ports.Added[i]
		kind := "port_up"
		if removed[p.Key()] {
			kind, restarted[p.Key()] = "port_restarted", true
		}
		events = append(events, state.Event{Kind: kind, At: at, Port: &p, Group: p.Group})
	}
	before := make(map[string]state.Port, len(prev.Ports))
	for _, p := range prev.Ports {
		before[p.Key()] = p
	}
	for _, key := range d.Ports.Removed {
		if restarted[key] {
			continue
		}
		p := before[key]
		events = append(events, state.Event{Kind: "port_down", At: at, Port: &p, Group: p.Group})
	}
	for i := range d.Ports.Updated {
		p := d.Ports.Updated[i]
		old := before[p.Key()]
		if healthStatus(old.Health) != healthStatus(p.Health) {
			events = append(events, state.Event{
				Kind: "health_changed", At: at, Port: &p, Group: p.Group,
				Data: map[string]any{"from": healthStatus(old.Health), "to": healthStatus(p.Health)},
			})
		}
	}
	return events
}

func healthStatus(h *state.Health) string {
	if h == nil {
		return ""
	}
	return h.Status
}

func (l *Loop) nowRFC3339() string { return l.now().Format(time.RFC3339) }

// Snapshot returns the current state for an RPC read. It reuses the cached
// scan when it is younger than CacheTTL and already carries everything the
// caller asked for; otherwise it scans now. Either way it wakes the loop, so
// reading state snaps the interval back to the base (spec, "Scanner loop").
func (l *Loop) Snapshot(include Include) (state.Snapshot, error) {
	snap, err := l.SnapshotAll(include)
	return localOnly(snap), err
}

// SnapshotAll is Snapshot including every registered remote host's rows. Only
// the state.snapshot method uses it, and it applies the caller's own `hosts`
// filter to the result.
func (l *Loop) SnapshotAll(include Include) (state.Snapshot, error) {
	defer l.Wake()

	if snap, ok := l.cached(include); ok {
		return snap, nil
	}
	return l.scanNow(include, mayCoalesce)
}

// Rescan scans now whatever the cache says, publishes the change it finds and
// only then returns. It is what a write path calls: `ports.rename`,
// `groups.assign` and the group config writes need the delta carrying the
// change to be on the wire before their own reply is (contract §18), and a
// kill needs its selectors resolved against what is listening this instant.
//
// It is not Invalidate + Snapshot. That pair has a window between the two
// calls: a tick landing in it refreshes lastScanAt from a scan that began
// before the write, Snapshot then finds the cache "fresh" and serves it, and
// the write reaches no one until the next tick — up to SubscribedMaxInterval
// later. Rescan closes the window by holding scanMu across both halves.
func (l *Loop) Rescan(include Include) (state.Snapshot, error) {
	defer l.Wake()
	snap, err := l.scanNow(include, forceScan)
	return localOnly(snap), err
}

// Republish makes a store or `.sonar.yaml` write visible on the wire without
// scanning the machine (contract §44).
//
// The write changed how the ports the daemon already knows are *named and
// grouped*, not which ports exist, so re-running attribution over the last
// scan's own OS rows answers it exactly. It takes the scan lock, so it still
// cannot interleave with a scan and §38's rule holds: a scan that started
// before the write commits first and this publish supersedes it, a scan that
// starts after it sees the write. What it no longer does is make a "Save"
// button wait for `lsof`, `ps` and `docker stats`.
//
// It wakes the loop on the way out, so a real scan follows and picks up
// anything that started or stopped meanwhile. Before the first scan, or if the
// lock cannot be had inside ScanLockBudget, it falls back to Rescan and to
// leaving the change for the next tick respectively; the second case means the
// daemon is wedged and is reported as an error rather than a hung reply.
func (l *Loop) Republish() error {
	if !lockFor(l.orderGate, ScanLockBudget) {
		l.Wake()
		return fmt.Errorf("scanner busy for more than %s; the change is saved and the next scan will publish it", ScanLockBudget)
	}
	l.mu.Lock()
	pp, have := l.lastPorts, l.haveSnap
	l.mu.Unlock()
	if !have || len(pp) == 0 {
		// Nothing to re-attribute yet: fall back to a real scan, which is
		// what this used to do unconditionally.
		unlock(l.orderGate)
		_, err := l.Rescan(Include{})
		return err
	}
	defer unlock(l.orderGate)
	defer l.Wake()

	subs, carry := l.opts.Demand()
	rows, groupRows := l.attribute(pp)
	sessionRows := l.sessions(rows)
	probeConfigured(rows, groupRows, l.opts.Probe, ConfiguredHealthBudget)

	l.commitMu.Lock()
	defer l.commitMu.Unlock()

	next, prev, changed := l.commitRepublish(subs, carry, rows, groupRows, sessionRows)
	if changed {
		l.publish(prev, next, deriveEvents(prev, next, l.nowRFC3339()))
	}
	return nil
}

// commitRepublish swaps a re-attributed snapshot into the cache. It is
// commitScan minus everything that belongs to an OS scan: it does not touch
// the scan counters, `lastScanAt`, the interval or the machine's load row, so
// the RPC cache TTL and `daemon status` read exactly what they would have if
// the write had never happened. Caller holds the ordering gate and commitMu.
func (l *Loop) commitRepublish(
	subs int, carry Include,
	rows []state.Port, groupRows []state.Group, sessionRows []state.SessionRecord,
) (next, prev state.Snapshot, changed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.subs = subs
	prev = l.snap

	carryHealth(prev.Ports, rows, true)
	carryStats(prev.Ports, rows)

	host := l.localhostRow(prev.Hosts)
	host.Ports, host.Groups = len(rows), len(groupRows)

	local := state.Rows{
		Ports:    rows,
		Groups:   groupRows,
		Sessions: sessionRows,
		Hosts:    []state.Host{host},
	}.Tag(state.LocalhostName).Normalize()

	next = local.Append(l.remoteRows()).Into(state.Snapshot{
		At:            l.now().Format(time.RFC3339),
		DaemonVersion: l.opts.DaemonVersion,
	})
	if !snapshotChanged(prev, next, true) && !hostChanged(prev, next) {
		// A write that changed nothing a client can see publishes nothing and
		// burns no seq, the same rule the stats tick follows.
		return prev, prev, false
	}

	l.local = local
	l.seq++
	next.Seq = l.seq
	l.snap = next
	return next, prev, true
}

// localhostRow is this machine's row as the last publisher left it, or the
// identity row when there is none.
func (l *Loop) localhostRow(hosts []state.Host) state.Host {
	for i := range hosts {
		if state.IsLocalhost(hosts[i].Name) {
			return hosts[i]
		}
	}
	return l.identityRow()
}

// scanMode says whether a caller may be given somebody else's scan.
type scanMode bool

const (
	// mayCoalesce is the read path. A scan that *began* after this call did
	// saw everything this caller could have seen, so waiting for the one in
	// flight and then running a second is pure duplication.
	mayCoalesce scanMode = false
	// forceScan is the write and kill path. `ports.kill` and `groups.kill`
	// resolve their selectors against the scan they ask for (contract §17,
	// §25) and a write's fallback republish has to see its own change, so
	// these always scan — never the one that happened to be in flight, and
	// never the one that finished a moment ago.
	forceScan scanMode = true
)

// scanNow is the uncached half of both: one scan under the run gate, published
// before it returns.
func (l *Loop) scanNow(include Include, mode scanMode) (state.Snapshot, error) {
	// Sampled before the gate: any scan that begins while this call waits
	// takes a higher number, which is what "began after I asked" means
	// without consulting a clock.
	asked := l.scanGeneration()
	if !lockFor(l.rpcGate, ScanLockBudget) {
		// Wedged: the last good snapshot beats a reply that never comes.
		l.opts.Logger.Warn("scanner busy, serving the last snapshot", "waited", ScanLockBudget)
		return l.CachedAll(), nil
	}
	defer unlock(l.rpcGate)

	if mode == mayCoalesce {
		if snap, ok := l.scannedSince(asked, include); ok {
			return snap, nil
		}
	}

	next, prev, _, err := l.scanLocked(include, l.demandInclude())
	if err != nil {
		if prev.Seq > 0 {
			// A failed rescan still serves the last good state.
			return prev, nil
		}
		return state.Snapshot{}, err
	}
	return next, nil
}

// beginScan numbers a scan as its OS half starts.
func (l *Loop) beginScan() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scanSeq++
	return l.scanSeq
}

// scanGeneration is the number of the last scan to have begun.
func (l *Loop) scanGeneration() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scanSeq
}

// scannedSince reports the cached snapshot when a scan that began strictly
// after generation gen has already committed one carrying what the caller
// asked for.
//
// Strictly after, and counted rather than timed: a scan numbered gen is the
// one that was already running (or had just finished) when the caller asked,
// and serving its result would be serving state from before the call. That
// distinction is invisible to `time.Now` on a platform whose clock moves in
// milliseconds, which is how a kill's rescan came to be skipped on Windows and
// nowhere else.
//
// Health never coalesces either way: the probes run on their own cadence, so
// "a scan happened" says nothing about whether it probed.
func (l *Loop) scannedSince(gen uint64, include Include) (state.Snapshot, bool) {
	if include.Health {
		return state.Snapshot{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.haveSnap || l.committedSeq <= gen {
		return state.Snapshot{}, false
	}
	if include.Stats && !l.snapHasStats() {
		return state.Snapshot{}, false
	}
	return l.snap, true
}

// cached returns the cached snapshot when it is younger than CacheTTL and
// already carries everything the caller asked for.
func (l *Loop) cached(include Include) (state.Snapshot, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fresh := l.haveSnap && l.now().Sub(l.lastScanAt) < CacheTTL
	covers := !include.Stats || l.snapHasStats()
	return l.snap, fresh && covers
}

// demandInclude is what the subscribers want a published snapshot to keep.
//
// A scan started by a read is published like any other, so it must not drop
// what a tick would have carried: without this a `sonar list` in the middle of
// a subscribed session would publish a snapshot with the stats stripped out,
// and every subscriber that opted into them would see them blink away and
// back. It used to be *collected* again, which put `docker stats` and a round
// of health probes on the critical path of every kill and every write; it is
// copied forward now instead (contract §44).
func (l *Loop) demandInclude() Include {
	_, want := l.opts.Demand()
	return want
}

// snapHasStats reports whether the cached snapshot carries stats. Caller holds
// the mutex.
func (l *Loop) snapHasStats() bool {
	for i := range l.snap.Ports {
		if l.snap.Ports[i].Stats != nil {
			return true
		}
	}
	return len(l.snap.Ports) == 0
}

// Status is what `daemon.status` reports about the scanner.
type Status struct {
	LastScanAt time.Time
	// IntervalMs is the adaptive cadence right now: somewhere between the
	// base and whichever ceiling currently applies.
	IntervalMs int
	// BaseIntervalMs and StatsIntervalMs are the effective settings behind
	// it: `daemon.scan_interval` and `daemon.stats_interval` after clamping.
	// Both are fixed for the life of the daemon, so `daemon.status` is where
	// you check that an edit to the config file took.
	BaseIntervalMs  int
	StatsIntervalMs int
	Scans           int64
	Seq             uint64
	LastError       error
}

// Status returns the scanner's current counters.
func (l *Loop) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{
		LastScanAt:      l.lastScanAt,
		IntervalMs:      int(l.interval / time.Millisecond),
		BaseIntervalMs:  int(l.base / time.Millisecond),
		StatsIntervalMs: int(l.statsInterval() / time.Millisecond),
		Scans:           l.scans,
		Seq:             l.seq,
		LastError:       l.lastErr,
	}
}
