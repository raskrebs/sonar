package groups

import (
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

// liveRegistry has one live run per name it is given, in the group "shop".
type liveRegistry struct {
	NoRuns
	live map[string]bool
}

func (r liveRegistry) ServiceLive(group, service string) bool {
	return group == "shop" && r.live[service]
}

// TestAServiceWithNoPortIsRunningWhileItsRunIsAlive: a worker never shows in a
// port scan, so the run sonar started for it is the only evidence it is up.
// Without it a project whose every service is up reads as "partial", with the
// worker "stopped". A service that does declare a port is not up until it
// listens, however alive its process is.
func TestAServiceWithNoPortIsRunningWhileItsRunIsAlive(t *testing.T) {
	index := NewIndex()
	index.Add(&Config{
		Name: "shop", Dir: "/nowhere/shop", Path: "/nowhere/shop/" + ConfigName,
		Services: []Service{
			{Name: "api", Cmd: "run-api", Port: 4000},
			{Name: "worker", Cmd: "run-worker"},
			{Name: "web", Cmd: "run-web", PortAuto: true},
		},
	})
	group, source := "shop", state.SourceFile
	listening := []state.Port{{Port: 4000, BindAddress: "127.0.0.1", Group: &group, GroupSource: &source}}

	rows := func(reg Registry) (map[string]state.Service, string) {
		gg := GroupsWith(listening, index, reg)
		if len(gg) != 1 {
			t.Fatalf("groups = %+v, want the one config's group", gg)
		}
		out := map[string]state.Service{}
		for _, s := range gg[0].Services {
			out[s.Name] = s
		}
		return out, gg[0].Status
	}

	got, _ := rows(liveRegistry{live: map[string]bool{"worker": true, "web": true}})
	if !got["api"].Running {
		t.Error("api listens on its declared port and should be running")
	}
	if !got["worker"].Running || got["worker"].PortActual != nil {
		t.Errorf("worker = running %v, port %v; want running with no port", got["worker"].Running, got["worker"].PortActual)
	}
	if got["web"].Running {
		t.Error("web declares a port and is not listening: a live run alone does not make it running")
	}

	got, _ = rows(liveRegistry{})
	if got["worker"].Running {
		t.Error("worker has no live run and should not be running")
	}

	// With no registry at all — the direct-scan path — nothing changes.
	got, _ = rows(nil)
	if got["worker"].Running {
		t.Error("worker is running with no registry to say so")
	}
}

// TestAProjectOfAPortAndAWorkerIsRunningNotPartial is the status that follows.
func TestAProjectOfAPortAndAWorkerIsRunningNotPartial(t *testing.T) {
	index := NewIndex()
	index.Add(&Config{
		Name: "shop", Dir: "/nowhere/shop", Path: "/nowhere/shop/" + ConfigName,
		Services: []Service{{Name: "api", Cmd: "run-api", Port: 4000}, {Name: "worker", Cmd: "run-worker"}},
	})
	group, source := "shop", state.SourceFile
	listening := []state.Port{{Port: 4000, BindAddress: "127.0.0.1", Group: &group, GroupSource: &source}}

	gg := GroupsWith(listening, index, liveRegistry{live: map[string]bool{"worker": true}})
	if len(gg) != 1 || gg[0].Status != "running" {
		t.Fatalf("status = %q, want running: both services are up", gg[0].Status)
	}
	gg = GroupsWith(listening, index, liveRegistry{})
	if gg[0].Status != "partial" {
		t.Fatalf("status = %q, want partial with the worker down", gg[0].Status)
	}
}
