package groups

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A reference is how a service's `cmd` and `env` name a port: its own with
// ${port} and ${url}, another service's with ${<service>.port} and
// ${<service>.url}. The daemon expands them when it starts the service, after
// the port of every service they name is known.
//
// Only those four shapes are references. Anything else between ${ and } —
// ${HOME}, ${FOO:-x} — is left exactly as written, because a `cmd` has never
// been expanded before and one that hands such a string to `sh -c` must keep
// working.
var refPattern = regexp.MustCompile(`\$\{([^{}$]*)\}`)

// envKey is what an `env:` key may be: a portable variable name.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	refPort = "port"
	refURL  = "url"
)

// parseRef reads the inside of ${...}. service is "" for the service's own
// port; ok is false for anything that is not a reference.
func parseRef(inner string) (service, field string, ok bool) {
	switch inner {
	case refPort, refURL:
		return "", inner, true
	}
	for _, f := range []string{refPort, refURL} {
		if svc, found := strings.CutSuffix(inner, "."+f); found && svc != "" {
			return svc, f, true
		}
	}
	return "", "", false
}

// URL is what ${url} expands to for a port.
func URL(port int) string { return "http://localhost:" + strconv.Itoa(port) }

// Expand replaces the references in text with the ports in ports, keyed by
// service name; self is the service text belongs to. A reference to a service
// ports has no entry for is left as written, and so is anything that is not a
// reference.
func Expand(text, self string, ports map[string]int) string {
	return refPattern.ReplaceAllStringFunc(text, func(m string) string {
		svc, field, ok := parseRef(m[2 : len(m)-1])
		if !ok {
			return m
		}
		if svc == "" {
			svc = self
		}
		port, ok := ports[svc]
		if !ok || port == 0 {
			return m
		}
		if field == refURL {
			return URL(port)
		}
		return strconv.Itoa(port)
	})
}

// Refs lists the services the references in text name, in order and without
// repeats; a bare ${port} or ${url} names self.
func Refs(text, self string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
		svc, _, ok := parseRef(m[1])
		if !ok {
			continue
		}
		if svc == "" {
			svc = self
		}
		if !seen[svc] {
			seen[svc] = true
			out = append(out, svc)
		}
	}
	return out
}

// MatchesExpanded reports whether actual could be pattern with its references
// expanded: the text around each reference must match exactly, and each
// reference stands for any non-empty text. `sonar start -- uvicorn --port
// 8123` matches a service whose cmd says `--port ${port}` this way.
func MatchesExpanded(pattern, actual string) bool {
	locs := refPattern.FindAllStringSubmatchIndex(pattern, -1)
	if len(locs) == 0 {
		return pattern == actual
	}
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range locs {
		b.WriteString(regexp.QuoteMeta(pattern[last:loc[0]]))
		if _, _, ok := parseRef(pattern[loc[2]:loc[3]]); ok {
			b.WriteString(".+")
		} else {
			b.WriteString(regexp.QuoteMeta(pattern[loc[0]:loc[1]]))
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(pattern[last:]))
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(actual)
}

// ServiceNamed returns the service with this name.
func (c *Config) ServiceNamed(name string) (Service, bool) {
	for _, s := range c.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// refProblems checks every reference in a service's cmd and env: each has to
// name a service in this file, and that service has to have a port.
func (c *Config) refProblems(s Service, where string) []string {
	var out []string
	check := func(field, text string) {
		for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
			svc, _, ok := parseRef(m[1])
			if !ok {
				continue
			}
			target, found := s, true
			if svc != "" {
				target, found = c.ServiceNamed(svc)
			}
			switch {
			case !found:
				out = append(out, fmt.Sprintf("%s: %s %s names no service %q", where, field, m[0], svc))
			case !target.HasPort():
				out = append(out, fmt.Sprintf("%s: %s %s needs a port, and %s declares none", where, field, m[0], target.Name))
			}
		}
	}
	check("cmd", s.Cmd)
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		check("env "+k, s.Env[k])
	}
	return out
}
