package daemon

import (
	"github.com/raskrebs/sonar/internal/claims"
	"github.com/raskrebs/sonar/internal/groups"
)

// AcquireServicePort claims the port a `port: auto` service runs on, for the
// checkout the service's `sonar.yaml` sits in (dir). It is the same claim
// `claims.acquire` makes, under claims.ServiceKey, so a claimed port is
// skipped by every other claim and by `ports.next`, and the claim is refreshed
// every time the service starts.
//
// It is exported for internal/daemon/groupstart, which starts services but
// cannot reach the claims book itself.
func AcquireServicePort(rt *Runtime, dir, service string) (int, error) {
	project, worktree := claims.Identity(dir, "", "")
	claimsMu.Lock()
	defer claimsMu.Unlock()
	m, err := claimsManager(rt)
	if err != nil {
		return 0, err
	}
	res, err := m.Acquire(claims.Request{
		Key:      claims.ServiceKey(project, worktree, service),
		Project:  project,
		Worktree: worktree,
		Count:    1,
	})
	if err != nil {
		return 0, claimsError("claiming a port for "+service, err)
	}
	return res.Ports[0], nil
}

// ReleaseServicePorts gives back the claims a config's `port: auto` services
// hold, for `sonar down`. It reports how many ports went.
func ReleaseServicePorts(rt *Runtime, cfg *groups.Config) (int, error) {
	if cfg == nil {
		return 0, nil
	}
	project, worktree := claims.Identity(cfg.Dir, "", "")
	claimsMu.Lock()
	defer claimsMu.Unlock()
	m, err := claimsManager(rt)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, svc := range cfg.Services {
		if !svc.PortAuto {
			continue
		}
		n, err := m.Release(claims.ServiceKey(project, worktree, svc.Name))
		if err != nil {
			return released, claimsError("releasing the port of "+svc.Name, err)
		}
		released += n
	}
	return released, nil
}

// configForGroup is the sonar.yaml whose services are published under a group
// name, or nil for a group no config describes.
func configForGroup(rt *Runtime, name string) *groups.Config {
	if rt.Scanner == nil {
		return nil
	}
	for _, cfg := range rt.Scanner.Configs() {
		if rt.Scanner.GroupOf(cfg) == name {
			return cfg
		}
	}
	return nil
}
