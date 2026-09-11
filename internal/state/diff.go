package state

import (
	"math"
	"reflect"
)

// Diff computes the delta from prev to next, ignoring Stats when deciding
// whether a port changed — a busy process must not produce a delta on every
// scan. A port whose PID changed is reported as a remove plus an add (a
// restart), never an update, so clients can tell "same server" from "new
// process on the same socket".
func Diff(prev, next Snapshot) Delta { return diff(prev, next, false) }

// DiffWithStats is Diff except that a stats-only change counts as an update.
// Used for subscribers that opted into stats.
func DiffWithStats(prev, next Snapshot) Delta { return diff(prev, next, true) }

func diff(prev, next Snapshot, withStats bool) Delta {
	return Delta{
		Seq:             next.Seq,
		At:              next.At,
		ExposuresActive: next.ExposuresActive,
		Ports:           diffPorts(prev.Ports, next.Ports, withStats),
		Groups: diffKeyed(prev.Groups, next.Groups,
			func(g Group) string { return g.Key() },
			func(a, b Group) bool {
				return a.Status == b.Status &&
					a.Repo == b.Repo && a.Worktree == b.Worktree && a.Branch == b.Branch &&
					reflect.DeepEqual(a.Members, b.Members) &&
					reflect.DeepEqual(a.Services, b.Services)
			}),
		Tunnels: diffKeyed(prev.Tunnels, next.Tunnels,
			func(t Tunnel) string { return t.Key() },
			func(a, b Tunnel) bool { return reflect.DeepEqual(a, b) }),
		Proxies: diffKeyed(prev.Proxies, next.Proxies,
			func(p Proxy) string { return p.Key() },
			func(a, b Proxy) bool { return reflect.DeepEqual(a, b) }),
		Sessions: diffKeyed(prev.Sessions, next.Sessions,
			func(s SessionRecord) string { return s.Key() },
			func(a, b SessionRecord) bool { return reflect.DeepEqual(a, b) }),
		Hosts: DiffHosts(prev.Hosts, next.Hosts),
	}
}

// DiffHosts is the `hosts` collection's diff, keyed by name. It is exported
// because the scanner asks the same question on its own — "did only the
// machine's load move?" — when it decides whether a tick is worth publishing.
func DiffHosts(prev, next []Host) Change[Host] {
	return diffKeyed(prev, next, func(h Host) string { return h.Key() }, hostsEqual)
}

// hostsEqual decides whether a host row changed. Host load is state, not an
// opt-in statistic: it is compared the same way for Diff and DiffWithStats, so
// a subscriber that asked for neither stats nor health still sees the machine's
// cpu and memory move (contract §22's rule for configured health).
//
// LastSeen and LatencyMs are excluded for the reason §25 excludes health
// latency: both move on every tick by construction, and comparing them would
// publish a `hosts` delta forever on a machine where nothing is happening. The
// newest values still ride out with the next real change.
//
// GPUs are compared at the resolution a meter shows: whole percent and whole
// MiB. Unified GPU memory moves by a few bytes on every read, and a delta for
// that would be noise; the precise value still rides out with the next real
// change. Null and [] stay distinct — "not collected" to "no GPU" is news.
func hostsEqual(a, b Host) bool {
	a.LastSeen, b.LastSeen = "", ""
	a.LatencyMs, b.LatencyMs = 0, 0
	a.GPUs, b.GPUs = gpusKey(a.GPUs), gpusKey(b.GPUs)
	return reflect.DeepEqual(a, b)
}

// gpusKey is the part of a GPU list a change is measured on.
func gpusKey(gpus []GPU) []GPU {
	if gpus == nil {
		return nil
	}
	out := make([]GPU, len(gpus))
	for i, g := range gpus {
		out[i] = GPU{Name: g.Name}
		if g.UtilizationPercent != nil {
			v := math.Round(*g.UtilizationPercent)
			out[i].UtilizationPercent = &v
		}
		out[i].MemoryUsedBytes = mib(g.MemoryUsedBytes)
		out[i].MemoryTotalBytes = mib(g.MemoryTotalBytes)
	}
	return out
}

func mib(v *int64) *int64 {
	if v == nil {
		return nil
	}
	m := *v >> 20
	return &m
}

// diffPorts keys ports by Key() and treats a PID change as remove + add.
func diffPorts(prev, next []Port, withStats bool) Change[Port] {
	before := make(map[string]Port, len(prev))
	for _, p := range prev {
		before[p.Key()] = p
	}

	ch := emptyChange[Port]()
	seen := make(map[string]bool, len(next))
	for _, p := range next {
		key := p.Key()
		seen[key] = true
		old, existed := before[key]
		switch {
		case !existed:
			ch.Added = append(ch.Added, p)
		case old.PID != p.PID:
			// Restart: the socket is the same but a different process owns it.
			ch.Removed = append(ch.Removed, key)
			ch.Added = append(ch.Added, p)
		case !portsEqual(old, p, withStats):
			ch.Updated = append(ch.Updated, p)
		}
	}
	for _, p := range prev {
		if !seen[p.Key()] {
			ch.Removed = append(ch.Removed, p.Key())
		}
	}
	return ch
}

// portsEqual compares two ports, optionally ignoring Stats. Health latency is
// always ignored: a probe that answers in 3 ms and then in 4 ms has not
// changed, and with configured health probed on every tick a live latency in
// the comparison would publish a delta every two seconds forever. The newest
// latency still rides out with the next real change.
func portsEqual(a, b Port, withStats bool) bool {
	if !withStats {
		a.Stats = nil
		b.Stats = nil
	}
	a.Health = healthKey(a.Health)
	b.Health = healthKey(b.Health)
	return reflect.DeepEqual(a, b)
}

// healthKey is the part of a health result a change is measured on.
func healthKey(h *Health) *Health {
	if h == nil {
		return nil
	}
	return &Health{Status: h.Status, Code: h.Code, Reason: h.Reason, Configured: h.Configured}
}

// diffKeyed is the generic collection diff used by every non-port collection.
func diffKeyed[T any](prev, next []T, key func(T) string, equal func(a, b T) bool) Change[T] {
	before := make(map[string]T, len(prev))
	for _, v := range prev {
		before[key(v)] = v
	}

	ch := emptyChange[T]()
	seen := make(map[string]bool, len(next))
	for _, v := range next {
		k := key(v)
		seen[k] = true
		old, existed := before[k]
		switch {
		case !existed:
			ch.Added = append(ch.Added, v)
		case !equal(old, v):
			ch.Updated = append(ch.Updated, v)
		}
	}
	for _, v := range prev {
		if !seen[key(v)] {
			ch.Removed = append(ch.Removed, key(v))
		}
	}
	return ch
}

// emptyChange returns a Change whose slices marshal as [] rather than null.
func emptyChange[T any]() Change[T] {
	return Change[T]{Added: []T{}, Updated: []T{}, Removed: []string{}}
}
