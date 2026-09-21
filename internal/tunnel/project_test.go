package tunnel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A project shared under one hostname, driven through a real tunnel.
//
// The routing table itself is internal/share's, and is tested there. What
// this covers is the half that only shows up on the wire: that the right
// service is dialled, that it is asked for the path it would see on
// localhost, and that it is told where it really sits.

// echoApp reports the request exactly as it received it, so a test can see
// what the service was actually asked for.
func echoApp(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s|%s", name, r.URL.RequestURI(), r.Host,
			r.Header.Get("X-Forwarded-Prefix"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAProjectIsRoutedByPathToTheRightService(t *testing.T) {
	fr := newFakeRelay(t)
	web := echoApp(t, "web")
	api := echoApp(t, "api")
	docs := echoApp(t, "docs")

	webAddr := strings.TrimPrefix(web.URL, "http://")
	apiAddr := strings.TrimPrefix(api.URL, "http://")
	docsAddr := strings.TrimPrefix(docs.URL, "http://")

	route := func(path string) Target {
		switch {
		case strings.HasPrefix(path, "/_sonar/api"):
			rest := strings.TrimPrefix(path, "/_sonar/api")
			if rest == "" {
				rest = "/"
			}
			return Target{Addr: apiAddr, Path: rest, Prefix: "/_sonar/api"}
		case strings.HasPrefix(path, "/docs"):
			// A service that expects to see its own prefix: nothing stripped,
			// so nothing to declare.
			return Target{Addr: docsAddr, Path: path}
		default:
			return Target{Addr: webAddr, Path: path}
		}
	}

	run := runClient(t, fr, Config{LocalPort: appPort(t, web), Route: route})
	run.waitState(t, StateConnected)
	rs := fr.wait(t)

	for _, tc := range []struct {
		ask        string
		service    string
		sees       string
		seesPrefix string
	}{
		// The frontend keeps everything nothing else claims, including the
		// paths a single-page application routes itself.
		{"/", "web", "/", ""},
		{"/dashboard/settings", "web", "/dashboard/settings", ""},
		// A mounted service is asked for the path it would see on localhost,
		// and told where it really sits.
		{"/_sonar/api/users", "api", "/users", "/_sonar/api"},
		{"/_sonar/api/users?q=1", "api", "/users?q=1", "/_sonar/api"},
		{"/_sonar/api", "api", "/", "/_sonar/api"},
		// And one that keeps its prefix gets it, with no header it did not
		// need.
		{"/docs/intro", "docs", "/docs/intro", ""},
	} {
		resp, body := rs.get(t, tc.ask)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s answered %d", tc.ask, resp.StatusCode)
			continue
		}
		parts := strings.Split(body, "|")
		if len(parts) != 4 {
			t.Errorf("%s gave %q", tc.ask, body)
			continue
		}
		if parts[0] != tc.service {
			t.Errorf("%s was answered by %q, want %q", tc.ask, parts[0], tc.service)
		}
		if parts[1] != tc.sees {
			t.Errorf("%s reached %s as %q, want %q", tc.ask, tc.service, parts[1], tc.sees)
		}
		if parts[3] != tc.seesPrefix {
			t.Errorf("%s gave %s X-Forwarded-Prefix %q, want %q",
				tc.ask, tc.service, parts[3], tc.seesPrefix)
		}
		// Every service is addressed as itself, or a dev server refuses the
		// Host outright.
		if !strings.Contains(parts[2], ":") {
			t.Errorf("%s left Host as %q", tc.ask, parts[2])
		}
	}
}

// A share of one service must take exactly the path it always did, so the
// table is only ever consulted when there is one.
func TestASingleServiceShareIsUnaffected(t *testing.T) {
	fr := newFakeRelay(t)
	app := echoApp(t, "only")
	run := runClient(t, fr, Config{LocalPort: appPort(t, app)})
	run.waitState(t, StateConnected)
	rs := fr.wait(t)

	resp, body := rs.get(t, "/_sonar/api/users")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	parts := strings.Split(body, "|")
	if parts[0] != "only" || parts[1] != "/_sonar/api/users" {
		t.Errorf("a single-service share rewrote the path: %q", body)
	}
	if parts[3] != "" {
		t.Errorf("a single-service share set X-Forwarded-Prefix to %q", parts[3])
	}
}
