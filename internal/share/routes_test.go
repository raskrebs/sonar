package share

import (
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

func svc(name string, port int) state.Service {
	p := port
	return state.Service{Name: name, Port: &p}
}

func withPath(s state.Service, path string) state.Service {
	p := path
	s.Path = &p
	return s
}

func withStrip(s state.Service, strip bool) state.Service {
	b := strip
	s.Strip = &b
	return s
}

func group(services ...state.Service) state.Group {
	return state.Group{Name: "aka-ai-platform", Services: services}
}

func mustRoutes(t *testing.T, g state.Group, entry string, skip ...string) Routes {
	t.Helper()
	m := map[string]bool{}
	for _, s := range skip {
		m[s] = true
	}
	r, err := buildRoutes(g, entry, m)
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	return r
}

// The default layout, and the reason it is not the obvious one.
func TestTheProjectLaysOutUnderOneReservedSegment(t *testing.T) {
	g := group(svc("web", 6873), svc("api", 9700), svc("admin", 9800))
	r := mustRoutes(t, g, "web")

	want := map[string]string{
		"/":             "web",
		"/_sonar/api":   "api",
		"/_sonar/admin": "admin",
	}
	if len(r) != len(want) {
		t.Fatalf("laid out %d services, want %d: %v", len(r), len(want), r.Table())
	}
	for _, rt := range r {
		if want[rt.Prefix] != rt.Service {
			t.Errorf("%s is %q, want %q", rt.Prefix, rt.Service, want[rt.Prefix])
		}
	}
}

// The whole reason for the reserved segment: the entry application usually
// serves paths named after the other services. /admin is a page in half the
// frontends anyone would share, and mounting the admin service there would
// silently shadow it.
func TestAServiceNeverShadowsAPathTheFrontendServes(t *testing.T) {
	g := group(svc("web", 6873), svc("admin", 9800))
	r := mustRoutes(t, g, "web")

	// The frontend's own /admin page is still the frontend's.
	route, path := r.Match("/admin")
	if route.Service != "web" {
		t.Errorf("/admin went to %q, want the frontend that serves it", route.Service)
	}
	if path != "/admin" {
		t.Errorf("the frontend was asked for %q, want /admin untouched", path)
	}

	// The admin service is reachable, somewhere the frontend will not be.
	route, path = r.Match("/_sonar/admin/users")
	if route.Service != "admin" {
		t.Errorf("/_sonar/admin/users went to %q", route.Service)
	}
	if path != "/users" {
		t.Errorf("the admin service was asked for %q, want /users", path)
	}
}

// The prefix is the share's addressing, not the application's: a service on
// :9700 that serves /users must still be asked for /users.
func TestAServiceIsAskedForThePathItWouldSeeOnLocalhost(t *testing.T) {
	g := group(svc("web", 6873), svc("api", 9700))
	r := mustRoutes(t, g, "web")

	for _, tc := range []struct{ in, want string }{
		{"/_sonar/api/users", "/users"},
		{"/_sonar/api/users?q=1", "/users?q=1"},
		// An application mounted at /api on its own port keeps that: the
		// visitor asks for /_sonar/api/api/users and it sees /api/users.
		{"/_sonar/api/api/users", "/api/users"},
		// The bare door and the bare door with a slash are the same door, and
		// a service asked for "" answers nothing useful.
		{"/_sonar/api", "/"},
		{"/_sonar/api/", "/"},
	} {
		route, got := r.Match(tc.in)
		if route.Service != "api" {
			t.Errorf("%s went to %q, want api", tc.in, route.Service)
			continue
		}
		if got != tc.want {
			t.Errorf("%s reached the api as %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A prefix matches on a segment boundary or not at all.
func TestAPrefixDoesNotSwallowALongerName(t *testing.T) {
	g := group(svc("web", 6873), svc("api", 9700))
	r := mustRoutes(t, g, "web")

	for _, path := range []string{"/_sonar/apiary", "/_sonar/api-docs", "/_sonarapi"} {
		if route, _ := r.Match(path); route.Service != "web" {
			t.Errorf("%s was taken by %q; only /_sonar/api and below belong to the api",
				path, route.Service)
		}
	}
}

// Anything nothing else claims is the frontend's, which is what makes an
// unknown path its business rather than a 404 from us — the behaviour every
// single-page application depends on.
func TestAnUnknownPathIsTheFrontendsBusiness(t *testing.T) {
	g := group(svc("web", 6873), svc("api", 9700))
	r := mustRoutes(t, g, "web")

	for _, path := range []string{"/", "/dashboard/settings", "/_sonar", "/_sonar/nope"} {
		route, got := r.Match(path)
		if route.Service != "web" {
			t.Errorf("%s went to %q, want the frontend", path, route.Service)
		}
		if got != path {
			t.Errorf("the frontend was asked for %q, want %q untouched", got, path)
		}
	}
}

// Longest prefix wins, so a service nested under another's path is reachable.
func TestTheLongestPrefixWins(t *testing.T) {
	g := group(
		svc("web", 6873),
		withPath(svc("api", 9700), "/api"),
		withPath(svc("v2", 9701), "/api/v2"),
	)
	r := mustRoutes(t, g, "web")

	if route, got := r.Match("/api/v2/users"); route.Service != "v2" || got != "/users" {
		t.Errorf("/api/v2/users went to %q as %q, want v2 as /users", route.Service, got)
	}
	if route, got := r.Match("/api/users"); route.Service != "api" || got != "/users" {
		t.Errorf("/api/users went to %q as %q, want api as /users", route.Service, got)
	}
}

func TestAProjectCanOverrideWhereAServiceSitsAndWhetherItIsStripped(t *testing.T) {
	g := group(
		svc("web", 6873),
		// A project that knows /api is free can have the short path.
		withPath(svc("api", 9700), "api/"),
		// And an application that does expect to see its prefix says so.
		withStrip(withPath(svc("docs", 9900), "/docs"), false),
	)
	r := mustRoutes(t, g, "web")

	if route, got := r.Match("/api/users"); route.Service != "api" || got != "/users" {
		t.Errorf("the override put api at %q as %q", route.Prefix, got)
	}
	if route, got := r.Match("/docs/intro"); route.Service != "docs" || got != "/docs/intro" {
		t.Errorf("strip:false gave docs %q, want the prefix left on", got)
	}
}

// Two services wanting one path is a mistake worth stopping at, not resolving
// silently — either answer would leave one of them unreachable.
func TestTwoServicesCannotClaimOnePath(t *testing.T) {
	g := group(
		svc("web", 6873),
		withPath(svc("api", 9700), "/thing"),
		withPath(svc("admin", 9800), "/thing"),
	)
	_, err := buildRoutes(g, "web", nil)
	if err == nil {
		t.Fatal("two services claimed one path and it was allowed")
	}
	if !strings.Contains(err.Error(), "path:") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}

// A database is left out entirely rather than becoming a path that can never
// work. The entry service must exist or there is no share to make.
func TestServicesThatCannotBeSharedAreLeftOut(t *testing.T) {
	g := group(svc("web", 6873), svc("api", 9700), svc("db", 5432))
	r := mustRoutes(t, g, "web", "db")

	for _, rt := range r {
		if rt.Service == "db" {
			t.Fatal("a service that cannot be shared was given a path")
		}
	}
	if route, _ := r.Match("/_sonar/db"); route.Service != "web" {
		t.Errorf("/_sonar/db went to %q; nothing should be mounted there", route.Service)
	}

	if _, err := buildRoutes(g, "nope", nil); err == nil {
		t.Error("a project whose entry service does not exist was laid out anyway")
	}
	// Including when the entry service is one that was skipped.
	if _, err := buildRoutes(g, "db", map[string]bool{"db": true}); err == nil {
		t.Error("a project entered at a service that cannot be shared was laid out anyway")
	}
}

// A service sonar started itself is wherever sonar put it, which is not what
// the file says.
func TestAServiceIsRoutedToWhereItIsActuallyListening(t *testing.T) {
	s := svc("api", 9700)
	actual := 34567
	s.PortActual = &actual
	r := mustRoutes(t, group(svc("web", 6873), s), "web")

	route, _ := r.Match("/_sonar/api/x")
	if route.Addr != "localhost:34567" {
		t.Errorf("the api is dialled at %q, want the port it is actually on", route.Addr)
	}
}

// A declared service that is not running has no port to dial. It is left out
// of the table rather than routed at :0.
func TestAServiceWithNoPortIsNotRouted(t *testing.T) {
	r := mustRoutes(t, group(svc("web", 6873), state.Service{Name: "worker"}), "web")
	if len(r) != 1 {
		t.Errorf("laid out %v, want the entry service alone", r.Table())
	}
}
