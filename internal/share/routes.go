package share

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/state"
)

// Where each service of a shared project sits under one hostname.
//
// A share is one hostname and one stream per request, and that is deliberate:
// the relay is never told the shape of the project, so it can never steer this
// daemon anywhere. The map from path to local port lives here, on the machine
// that already knows both.
//
// The entry service is at the root. Every other service sits under a single
// reserved segment:
//
//	/                     web    :6873
//	/_sonar/api           api    :9700
//	/_sonar/admin         admin  :9800
//
// The reserved segment is the whole reason this is not simply `/api`. The
// entry application usually serves paths named after the other services —
// `/admin` is a page in half the frontends anyone would share — and mounting a
// service there silently shadows a real route. Checking first does not help:
// a single-page application answers 200 on every path, and those are exactly
// the applications people share. One segment a project must avoid is a cost
// that can be stated once; one segment per service is a trap that fires later.
//
// A project that knows a path is free can say so per service in `sonar.yaml`.
const reservedSegment = "_sonar"

// Route is one service's place under the share's hostname.
type Route struct {
	// Service is the name from sonar.yaml, for the table the CLI prints.
	Service string
	// Prefix is what the visitor's path starts with. "/" for the entry
	// service, which matches everything nothing else claims.
	Prefix string
	// Addr is the local address to dial, "localhost:9700".
	Addr string
	// Strip removes Prefix before the request reaches the service, so the
	// service sees the path it would see on localhost. On by default, because
	// the prefix is the share's addressing rather than the application's: a
	// service on :9700 serving /users is reached at /_sonar/api/users and must
	// still be asked for /users.
	//
	// Off for the entry service, whose prefix is "/" and has nothing to strip,
	// and for a service whose sonar.yaml says `strip: false` because the
	// application does expect to see the prefix.
	Strip bool
}

// Routes is a project's table, longest prefix first so that the first match
// is the right one.
type Routes []Route

// Entry is the service at the root.
func (r Routes) Entry() (Route, bool) {
	for _, rt := range r {
		if rt.Prefix == "/" {
			return rt, true
		}
	}
	return Route{}, false
}

// Match picks the service for one request path, and returns the path to ask
// that service for.
//
// Longest prefix wins, and a prefix only matches on a segment boundary:
// /_sonar/apixyz is not inside /_sonar/api. Nothing matching falls to the
// entry service, which is what makes an unknown path the frontend's business
// rather than a 404 from us.
func (r Routes) Match(path string) (Route, string) {
	for _, rt := range r {
		if rt.Prefix == "/" {
			continue
		}
		if !underPrefix(path, rt.Prefix) {
			continue
		}
		if !rt.Strip {
			return rt, path
		}
		rest := strings.TrimPrefix(path, rt.Prefix)
		if rest == "" {
			// /_sonar/api and /_sonar/api/ are the same door, and a service
			// asked for "" rather than "/" answers nothing useful.
			rest = "/"
		}
		return rt, rest
	}
	entry, ok := r.Entry()
	if !ok {
		return Route{}, path
	}
	return entry, path
}

// underPrefix reports whether path is prefix or sits beneath it. The segment
// boundary is the whole of it: without the check, /_sonar/apiary would be
// handed to the service mounted at /_sonar/api.
func underPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/' || rest[0] == '?'
}

// Table is what the CLI prints under the URL.
func (r Routes) Table() []string {
	out := make([]string, 0, len(r))
	for _, rt := range r {
		out = append(out, fmt.Sprintf("%s  %s", rt.Prefix, rt.Service))
	}
	return out
}

// buildRoutes lays a group's services out under one hostname.
//
// entry names the service at the root — the one whose port the person shared,
// or the one their sonar.yaml puts there. Every service in `skip` is left out
// entirely; that is how a database or anything else that does not speak HTTP
// stays out of a project share instead of becoming a path that cannot work.
func buildRoutes(g state.Group, entry string, skip map[string]bool) (Routes, error) {
	var routes Routes
	seen := map[string]string{} // prefix -> service that claimed it

	claim := func(prefix, service string) error {
		if other, taken := seen[prefix]; taken {
			return fmt.Errorf("%q and %q both want %s; give one of them a different `path:` in sonar.yaml",
				other, service, prefix)
		}
		seen[prefix] = service
		return nil
	}

	for _, svc := range g.Services {
		if skip[svc.Name] {
			continue
		}
		port := servicePort(svc)
		if port == 0 {
			// Declared but not running. A share cannot carry it, and saying so
			// is the CLI's job; there is nothing to route.
			continue
		}
		addr := net.JoinHostPort("localhost", strconv.Itoa(port))

		if svc.Name == entry {
			if err := claim("/", svc.Name); err != nil {
				return nil, err
			}
			routes = append(routes, Route{Service: svc.Name, Prefix: "/", Addr: addr})
			continue
		}
		prefix, strip := servicePath(svc)
		if err := claim(prefix, svc.Name); err != nil {
			return nil, err
		}
		routes = append(routes, Route{Service: svc.Name, Prefix: prefix, Addr: addr, Strip: strip})
	}

	if _, ok := routes.Entry(); !ok {
		return nil, fmt.Errorf("%q is not a service of this project, so nothing would answer at /", entry)
	}

	// Longest first, so Match can take the first hit. Ties broken by name so
	// the table a person reads is stable between runs.
	sort.SliceStable(routes, func(i, j int) bool {
		if len(routes[i].Prefix) != len(routes[j].Prefix) {
			return len(routes[i].Prefix) > len(routes[j].Prefix)
		}
		return routes[i].Service < routes[j].Service
	})
	return routes, nil
}

// servicePath is where a non-entry service sits, and whether its prefix is
// taken off on the way in.
//
// The default is the reserved segment and a strip, which is the combination
// that makes a service reachable without knowing anything about it. A
// sonar.yaml `path:` overrides where; `strip: false` overrides whether.
func servicePath(svc state.Service) (string, bool) {
	prefix := "/" + reservedSegment + "/" + svc.Name
	strip := true
	if svc.Path != nil && strings.TrimSpace(*svc.Path) != "" {
		prefix = normalizePrefix(*svc.Path)
	}
	if svc.Strip != nil {
		strip = *svc.Strip
	}
	return prefix, strip
}

// normalizePrefix makes a hand-written `path:` into one this can match on: a
// leading slash, no trailing one, no doubled separators.
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		p = "/"
	}
	return p
}

// servicePort is where a service is actually listening: the port sonar
// assigned if it started it, otherwise the one the file declares.
func servicePort(svc state.Service) int {
	if svc.PortActual != nil && *svc.PortActual > 0 {
		return *svc.PortActual
	}
	if svc.Port != nil && *svc.Port > 0 {
		return *svc.Port
	}
	return 0
}
