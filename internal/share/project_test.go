package share

import (
	"context"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// A project as the daemon sees one: a group with a committed config, and ports
// listening for each service.
func projectSnapshot(t *testing.T, services ...state.Service) state.Snapshot {
	t.Helper()
	var ports []state.Port
	for _, s := range services {
		if s.Port == nil {
			continue
		}
		ports = append(ports, state.Port{
			Host: state.LocalhostName, Port: *s.Port, PID: 100 + *s.Port,
			Cwd: "/src/acme", Group: ptr("acme"), ProjectRoot: ptr("/src/acme"),
		})
	}
	root := "/src/acme"
	return state.Snapshot{
		Ports: ports,
		Groups: []state.Group{{
			Host: state.LocalhostName, Name: "acme", Repo: "acme",
			RootDir: &root, Services: services,
		}},
	}
}

// Everything that speaks HTTP is carried; everything that does not is left out
// by name, so nobody wonders where the database went.
func TestAProjectCarriesItsWebServicesAndNamesWhatItLeftBehind(t *testing.T) {
	e := newEnv(t)
	snap := projectSnapshot(t, svc("web", 6873), svc("api", 9700), svc("db", 5432))
	e.m.probeFn = func(_ context.Context, addr string) (probeResult, error) {
		if strings.HasSuffix(addr, ":5432") {
			return probeResult{verdict: spokeFirst}, nil
		}
		return probeResult{verdict: speaksHTTP, contentType: "text/html"}, nil
	}

	got, err := e.m.Preview(context.Background(), snap,
		rpc.ShareProjectParams{Target: rpc.Selector{Port: ptr(6873)}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available {
		t.Fatalf("a project with a web service and an api was not offered: %q", got.Reason)
	}
	if got.Entry != "web" {
		t.Errorf("the entry service is %q, want the one whose port was named", got.Entry)
	}
	joined := strings.Join(got.Services, "\n")
	// The entry service first, then the rest, then what was left behind.
	if !strings.HasPrefix(joined, "/  ") {
		t.Errorf("the table does not start at the root:\n%s", joined)
	}
	for _, want := range []string{"web", "/_sonar/api", "api", "db (does not speak HTTP)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the description is missing %q:\n%s", want, joined)
		}
	}
}

// Decision 13: without a committed config there is no stable name to key the
// address on, so it could not survive the project moving.
func TestAProjectWithNoCommittedConfigIsRefusedWithTheFix(t *testing.T) {
	e := newEnv(t)
	root := "/src/loose"
	snap := state.Snapshot{
		Ports: []state.Port{{
			Host: state.LocalhostName, Port: 6873, PID: 1, Cwd: root,
			Group: ptr("loose"), ProjectRoot: &root,
		}},
		// A group with no Repo is one the daemon inferred, not one a
		// committed sonar.yaml declared.
		Groups: []state.Group{{Host: state.LocalhostName, Name: "loose", RootDir: &root}},
	}
	e.m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: speaksHTTP}, nil
	}

	got, err := e.m.Preview(context.Background(), snap,
		rpc.ShareProjectParams{Target: rpc.Selector{Port: ptr(6873)}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Fatal("a project with no committed config was offered anyway")
	}
	if !strings.Contains(got.Reason, "sonar.yaml") {
		t.Errorf("the refusal does not name what is missing: %q", got.Reason)
	}
}

// A lone service is not a project, and offering to share "the whole project"
// when there is nothing else would be noise.
func TestALoneServiceIsNotOfferedAsAProject(t *testing.T) {
	e := newEnv(t)
	snap := projectSnapshot(t, svc("web", 6873))
	e.m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: speaksHTTP}, nil
	}

	got, err := e.m.Preview(context.Background(), snap,
		rpc.ShareProjectParams{Target: rpc.Selector{Port: ptr(6873)}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Errorf("a project of one service was offered: %v", got.Services)
	}
}

// Sharing `web` and sharing the whole project are different things and must
// keep different addresses, or a link sent on Monday serves something else on
// Tuesday.
func TestAProjectShareDoesNotTakeTheServicesAddress(t *testing.T) {
	e := newEnv(t)
	e.relay.limit = 9
	snap := projectSnapshot(t, svc("web", 6873), svc("api", 9700))
	e.m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: speaksHTTP, contentType: "text/html"}, nil
	}
	ctx := context.Background()

	one, _, err := e.m.Create(ctx, snap, createParams(6873, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	p := createParams(6873, ReachPublic)
	p.Project = true
	whole, notes, err := e.m.Create(ctx, snap, p)
	if err != nil {
		t.Fatal(err)
	}

	if one.URL == whole.URL {
		t.Errorf("the project share took the service's address: %s", one.URL)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "/_sonar/api") {
		t.Errorf("the project share did not say where its services sit: %v", notes)
	}
}
