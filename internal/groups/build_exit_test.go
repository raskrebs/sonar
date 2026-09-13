package groups

import (
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

// exitRegistry knows how one service ended and nothing else.
type exitRegistry struct{ NoRuns }

func (exitRegistry) LastExit(group, service string) (state.ServiceExit, bool) {
	if group == "shop" && service == "api" {
		return state.ServiceExit{Code: 1, Reason: "crashed", At: "2026-09-13T10:00:00Z", RunID: "a"}, true
	}
	return state.ServiceExit{}, false
}

// TestGroupsCarryTheLastExitOfAServiceThatIsDown: a stopped service says how
// it ended, so a client can show "crashed" rather than only "not running".
func TestGroupsCarryTheLastExitOfAServiceThatIsDown(t *testing.T) {
	index := NewIndex()
	index.Add(&Config{
		Name: "shop", Dir: "/nowhere/shop", Path: "/nowhere/shop/" + ConfigName,
		Services: []Service{{Name: "api", Cmd: "run-api"}, {Name: "web", Cmd: "run-web"}},
	})

	gg := GroupsWith(nil, index, exitRegistry{})
	if len(gg) != 1 {
		t.Fatalf("groups = %+v, want the one config's group", gg)
	}
	byName := map[string]state.Service{}
	for _, s := range gg[0].Services {
		byName[s.Name] = s
	}
	api, web := byName["api"], byName["web"]
	if api.LastExit == nil || api.LastExit.Code != 1 || api.LastExit.Reason != "crashed" {
		t.Errorf("api.LastExit = %+v, want the crash", api.LastExit)
	}
	if web.LastExit != nil {
		t.Errorf("web.LastExit = %+v, want nothing for a service that never ran", web.LastExit)
	}
}

// TestGroupsWithoutARegistryHaveNoExits: the direct-scan path has no registry
// at all, and must still build its groups.
func TestGroupsWithoutARegistryHaveNoExits(t *testing.T) {
	index := NewIndex()
	index.Add(&Config{
		Name: "shop", Dir: "/nowhere/shop", Path: "/nowhere/shop/" + ConfigName,
		Services: []Service{{Name: "api", Cmd: "run-api"}},
	})
	gg := Groups(nil, index)
	if len(gg) != 1 || len(gg[0].Services) != 1 || gg[0].Services[0].LastExit != nil {
		t.Fatalf("groups = %+v", gg)
	}
}
