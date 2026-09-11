package scanner

import (
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
)

// maxHealthProbes bounds how many configured health checks run at once, the
// same ceiling the opt-in probe uses.
const maxHealthProbes = ports.MaxProbes

// Probe is one health check. Options.Probe replaces it for tests; production
// is ports.ProbeHealth.
type Probe func(host string, port int, path string, timeout time.Duration) ports.HealthResult

// probeConfigured fills Port.health for every `sonar.yaml` service that
// declares a `health:` path and whose port is listening.
//
// Unlike the opt-in `include: ["health"]` probe, this one runs on every tick
// and for every subscriber: a health path in a config is a statement about what
// the service *is*, not a statistic a client may or may not want, so the
// daemon polls it as part of state (step 1A.7). Everything else about it is the
// same probe — one HTTP GET, HealthTimeout, ten in flight.
func probeConfigured(rows []state.Port, gg []state.Group, probe Probe, budget time.Duration) {
	targets := healthTargets(rows, gg)
	if len(targets) == 0 {
		return
	}

	// The whole round is bounded, not just one probe. This runs inside the
	// scan, which every write handler's republish and every kill's rescan is
	// queued behind (contract §38), so a project that declares health paths on
	// a dozen services — half of them accepting and never answering — must not
	// be able to add waves of HealthTimeout to somebody's "Save" button.
	var deadline time.Time
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}

	results := make([]state.Health, len(targets))
	probed := make([]bool, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxHealthProbes)
	for i := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			timeout, ok := ports.ProbeBudget(HealthTimeout, deadline)
			if !ok {
				return
			}
			t := targets[i]
			r := probe(t.host, t.port, t.path, timeout)
			status, reason := state.NormalizeHealth(r.Status)
			results[i] = state.Health{
				Status:     status,
				Code:       r.StatusCode,
				LatencyMs:  r.Latency.Milliseconds(),
				Reason:     reason,
				Configured: true,
			}
			probed[i] = true
		}(i)
	}
	wg.Wait()

	for i, t := range targets {
		if !probed[i] {
			// Left for carryHealth: the previous tick's verdict beats a
			// flicker to "unknown" for a probe that never ran.
			continue
		}
		h := results[i]
		for _, idx := range t.rows {
			row := h
			rows[idx].Health = &row
		}
	}
}

// healthTarget is one configured probe: the port, the path from the config and
// the rows the result is written back to.
type healthTarget struct {
	port int
	host string
	path string
	rows []int
}

// healthTargets joins the configured health paths against the ports actually
// listening. A service whose port is down has no row to carry a result, so it
// simply is not probed; the group already reports it as not running.
func healthTargets(rows []state.Port, gg []state.Group) []healthTarget {
	paths := map[int]string{}
	for _, g := range gg {
		for _, svc := range g.Services {
			if svc.Health == nil || *svc.Health == "" || svc.PortActual == nil {
				continue
			}
			if _, taken := paths[*svc.PortActual]; !taken {
				paths[*svc.PortActual] = *svc.Health
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}

	byPort := map[int]*healthTarget{}
	out := []healthTarget{}
	order := []int{}
	for i := range rows {
		path, want := paths[rows[i].Port]
		if !want {
			continue
		}
		t, seen := byPort[rows[i].Port]
		if !seen {
			t = &healthTarget{port: rows[i].Port, path: path}
			byPort[rows[i].Port] = t
			order = append(order, rows[i].Port)
		}
		t.rows = append(t.rows, i)
		t.host = preferHost(t.host, rows[i])
	}
	for _, port := range order {
		out = append(out, *byPort[port])
	}
	return out
}

// preferHost picks the loopback address to probe. A service bound on IPv4 (or
// on every address) is reached at 127.0.0.1; one that only ever bound IPv6 is
// reached at [::1]. Picking per port rather than per row is what keeps a
// dual-stack listener from being probed twice with two different answers.
func preferHost(current string, p state.Port) string {
	if current == "127.0.0.1" {
		return current
	}
	switch p.BindAddress {
	case "::", "::1", "[::]", "[::1]":
		if current == "" {
			return "[::1]"
		}
		return current
	default:
		return "127.0.0.1"
	}
}
