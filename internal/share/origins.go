package share

import (
	"fmt"
	"sort"
	"strings"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/state"
)

// Telling someone their application will refuse the share it is now serving.
//
// A shared application often checks the origin of the page talking to it —
// a CORS allowlist, a sign-in redirect allowlist, a dev server's list of
// hostnames it will answer for. On localhost those lists contain localhost,
// which is right, and through a share they are wrong in a way that produces
// an error naming CORS or a redirect rather than naming the list. The
// observed case: sign-in returned 400 "invalid redirect_uri" because the
// share's address was not on the backend's list, and nothing in the error
// said so.
//
// sonar cannot fix this. It does not know which variable a given framework
// reads — BACKEND_CORS_ORIGINS, ALLOWED_HOSTS, allowedDevOrigins and
// CORS_ORIGIN are all in use and there is no standard — and it will not edit
// a project's configuration or restart its services to find out. What it can
// do is read the project's own `sonar.yaml`, find the variables that look
// like such a list and still name only localhost, and say which one and what
// to add.
//
// The value of that is precision: not "you may need to configure CORS", which
// anyone could have guessed, but the name of the variable in front of them.

// originHints are the substrings that make a variable name look like a list of
// origins or hostnames a service will accept.
//
// Deliberately few. Every one of these was chosen from a framework that is
// actually used — FastAPI, Django, Next.js, Vite, Rails — and a name that
// matched loosely would spend the one line this gets on a false alarm.
var originHints = []string{
	"CORS",           // BACKEND_CORS_ORIGINS, CORS_ORIGIN, CORS_ALLOWED_ORIGINS
	"ALLOWED_ORIGIN", // ALLOWED_ORIGINS
	"ALLOWED_HOST",   // Django's ALLOWED_HOSTS
	"ORIGIN",         // allowedDevOrigins, TRUSTED_ORIGINS
	"FRONTEND_HOST",  // the redirect target a backend sends people back to
	"FRONTEND_URL",
	"PUBLIC_URL",
	"SITE_URL",
}

// originAdvice is one service's variable that will refuse this share.
type originAdvice struct {
	Service  string
	Variable string
}

// originsToUpdate finds the variables in a project's config that name only
// localhost and will therefore turn this share away.
//
// Read from the committed sonar.yaml rather than from the daemon's snapshot,
// because the snapshot deliberately does not carry a service's environment:
// those values hold API keys and passwords, and publishing them to every
// client that subscribes would be a far worse bug than the one this fixes.
// Only the variable's name is ever shown, never its value.
//
// Only variables whose value already mentions localhost are reported. One that
// names a real hostname is somebody's deliberate configuration and is not
// ours to comment on; one that is empty is not a list.
func originsToUpdate(g state.Group) []originAdvice {
	if g.ConfigPath == nil || strings.TrimSpace(*g.ConfigPath) == "" {
		return nil
	}
	cfg, err := groups.Load(*g.ConfigPath)
	if err != nil || cfg == nil {
		// A project whose config cannot be read still gets a share; it just
		// gets no advice about it.
		return nil
	}

	var out []originAdvice
	for _, svc := range cfg.Services {
		for name, value := range svc.Env {
			if !looksLikeOriginList(name) || !namesOnlyLocalhost(value) {
				continue
			}
			out = append(out, originAdvice{Service: svc.Name, Variable: name})
		}
	}
	// Stable, so the same project says the same thing twice running.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Variable < out[j].Variable
	})
	return out
}

// looksLikeOriginList matches a hint only at a word boundary — the start of
// the name, or just after an underscore.
//
// A plain substring match is not good enough: DATABASE_URL contains BASE_URL,
// and one false alarm makes the whole line worth ignoring. BASE_URL itself was
// dropped for the same reason — API_BASE_URL is where a client points, not a
// list of who may ask.
func looksLikeOriginList(name string) bool {
	upper := strings.ToUpper(name)
	for _, hint := range originHints {
		for i := 0; ; {
			at := strings.Index(upper[i:], hint)
			if at < 0 {
				break
			}
			at += i
			if at == 0 || upper[at-1] == '_' {
				return true
			}
			i = at + 1
		}
	}
	return false
}

// namesOnlyLocalhost reports whether every address in a value is a loopback
// one. A list that already carries a real hostname has been thought about.
func namesOnlyLocalhost(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	seen := false
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	}) {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		// A reference sonar has not expanded yet still points at localhost:
		// ${url} and ${<service>.url} both expand to http://localhost:<port>.
		if strings.Contains(part, "${") {
			part = strings.ReplaceAll(part, "${url}", "http://localhost")
		}
		if !strings.Contains(part, "localhost") && !strings.Contains(part, "127.0.0.1") &&
			!strings.Contains(part, "[::1]") {
			return false
		}
		seen = true
	}
	return seen
}

// originNote is the sentence printed under a share's URL, or empty when there
// is nothing specific to say.
func originNote(g state.Group, url string) string {
	advice := originsToUpdate(g)
	if len(advice) == 0 || strings.TrimSpace(url) == "" {
		return ""
	}

	var b strings.Builder
	if len(advice) == 1 {
		fmt.Fprintf(&b, "%s reads %s, and it lists only localhost. ",
			advice[0].Service, advice[0].Variable)
	} else {
		names := make([]string, 0, len(advice))
		for _, a := range advice {
			names = append(names, a.Service+"'s "+a.Variable)
		}
		fmt.Fprintf(&b, "%s list only localhost. ", strings.Join(names, ", "))
	}
	b.WriteString("Signing in, and anything else that checks which page is " +
		"asking, will refuse this share until they include:\n\n    ")
	b.WriteString(url)
	b.WriteString("\n\nThe address does not change, so this is once.")
	return b.String()
}
