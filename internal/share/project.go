package share

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// Sharing a whole project rather than one of its services.
//
// The shape is decided: one hostname, the entry service at the root, every
// other HTTP service under a reserved path (see routes.go). What is here is
// how a project becomes that — which service is the entry, which services are
// left out, and the two refusals that are better than a share which cannot
// work.

// projectSentinel is the service name a project share is keyed on.
//
// A share's key is (account, repo, worktree, service), and a project share
// needs a slot in that space of its own: sharing `web` and sharing the whole
// project are different things and must not hand back each other's URL — a
// link sent on Monday has to serve on Tuesday what it served on Monday.
//
// A sentinel rather than an empty service, because the store refuses an empty
// one outright (ShareKey.Valid), and that refusal is worth keeping: it is what
// catches a caller that forgot to name a service at all. `*` cannot collide
// with a real name — sonar.yaml service names are identifiers.
const projectSentinel = "*"

// project is a whole project resolved into something shareable.
type project struct {
	// Group is the daemon's group name, for the sentence a person reads.
	Group string
	// Entry is the service at the root.
	Entry string
	// Routes is the table: every service that is in, and where it sits.
	Routes Routes
	// Excluded names the services left out and why, so the output can say
	// where the database went rather than leaving someone to wonder.
	Excluded []excluded
	// The committed key. A project share has no fallback form — see
	// resolveProject.
	Repo     string
	Worktree string
}

type excluded struct {
	Service string
	Why     string
}

// Other reports whether this project has anything beyond its entry service,
// which is the whole question of whether to offer a project share at all.
func (p project) Other() bool { return len(p.Routes) > 1 }

// resolveProject works out what sharing the whole of a port's project would
// mean, without doing it.
//
// The two refusals are deliberate and are the decisions taken on 2026-09-21:
//
//   - No committed sonar.yaml, no project share. A share's address is meant to
//     follow the project rather than the folder it happens to sit in, and
//     without a committed file there is no stable name to key one on — it
//     could not survive being cloned elsewhere, which is the promise.
//   - A service that does not speak HTTP is left out rather than given a path
//     that could never answer.
func (m *Manager) resolveProject(ctx context.Context, snap state.Snapshot, t target) (project, error) {
	if strings.TrimSpace(t.Group) == "" {
		return project{}, rpc.NewError(rpc.CodeInvalidSelector,
			"this port is not part of a project, so there is nothing to share but the port itself",
			"`sonar share "+strconv.Itoa(t.Port)+" --public` shares it on its own")
	}
	g, ok := groupNamed(snap, t.Group)
	if !ok {
		return project{}, rpc.Errorf(rpc.CodeNotFound, "no project named %q is running", t.Group)
	}
	if strings.TrimSpace(t.Repo) == "" || strings.TrimSpace(t.Service) == "" {
		// Decision 13. Said in full, because the fix is one command and the
		// alternative is an address that changes under them later.
		return project{}, rpc.NewError(rpc.CodeInvalidConfig,
			"sharing a whole project needs a committed sonar.yaml, so the address can follow "+
				"the project rather than this folder",
			"run `sonar init`, commit the file, and try again — or share just this port, "+
				"which needs nothing")
	}

	p := project{Group: g.Name, Entry: t.Service, Repo: t.Repo, Worktree: t.Worktree}

	// Ask every other service what it is. The entry service is already known
	// to speak HTTP: it was probed before this, on the path that shares one
	// port.
	skip := map[string]bool{}
	for _, svc := range g.Services {
		if svc.Name == p.Entry {
			continue
		}
		port := servicePort(svc)
		if port == 0 {
			skip[svc.Name] = true
			p.Excluded = append(p.Excluded, excluded{svc.Name, "not running"})
			continue
		}
		res, err := m.probeFn(ctx, net.JoinHostPort("localhost", strconv.Itoa(port)))
		if err != nil {
			skip[svc.Name] = true
			p.Excluded = append(p.Excluded, excluded{svc.Name, "not answering"})
			continue
		}
		if !res.verdict.ok() {
			skip[svc.Name] = true
			p.Excluded = append(p.Excluded, excluded{svc.Name, whyNotHTTP(res.verdict)})
			continue
		}
	}
	sort.SliceStable(p.Excluded, func(i, j int) bool { return p.Excluded[i].Service < p.Excluded[j].Service })

	routes, err := buildRoutes(g, p.Entry, skip)
	if err != nil {
		return project{}, rpc.Errorf(rpc.CodeInvalidConfig, "%s", err.Error())
	}
	p.Routes = routes
	return p, nil
}

// whyNotHTTP is the short reason a service is left out, for the list printed
// under the URL. The long version, with what to do instead, belongs to the
// refusal a person gets when they share that port on its own.
func whyNotHTTP(v verdict) string {
	switch v {
	case speaksTLS:
		return "speaks HTTPS"
	default:
		return "does not speak HTTP"
	}
}

// key is the reservation a project share is published under.
func (p project) key() target {
	return target{
		Group:    p.Group,
		Repo:     p.Repo,
		Worktree: p.Worktree,
		Service:  projectSentinel,
		Port:     p.entryPort(),
	}
}

func (p project) entryPort() int {
	entry, ok := p.Routes.Entry()
	if !ok {
		return 0
	}
	_, port, err := net.SplitHostPort(entry.Addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

// describe is the table printed under the URL, plus the services left out.
//
// One note rather than several, because it is a table: a client that wraps
// each note as its own paragraph would otherwise space the rows apart and
// lose the shape that makes it readable.
func (p project) describe() []string {
	if len(p.Routes) == 0 {
		return nil
	}
	width := 0
	for _, r := range p.Routes {
		if len(r.Prefix) > width {
			width = len(r.Prefix)
		}
	}
	// Reading order, not routing order: the table is sorted longest-prefix
	// first so that Match can take the first hit, which puts the entry
	// service last. A person reads down from the root.
	shown := slices.Clone(p.Routes)
	slices.SortStableFunc(shown, func(a, b Route) int {
		if (a.Prefix == "/") != (b.Prefix == "/") {
			if a.Prefix == "/" {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Prefix, b.Prefix)
	})

	var b strings.Builder
	for _, r := range shown {
		fmt.Fprintf(&b, "%-*s  %s\n", width, r.Prefix, r.Service)
	}
	for _, e := range p.Excluded {
		fmt.Fprintf(&b, "%-*s  %s (%s)\n", width, "", e.Service, e.Why)
	}
	table := strings.TrimRight(b.String(), "\n")
	return []string{table, "Point your frontend at those paths, relative. They will be right " +
		"for every share you ever publish, and on localhost too."}
}

// Preview is `share.project`: what sharing the whole of a target's project
// would look like, without publishing anything.
//
// It exists so the CLI can ask before it creates. The alternative — publish
// the one service, then offer to replace it — spends a slug and leaves a share
// live that nobody asked for, however briefly.
func (m *Manager) Preview(ctx context.Context, snap state.Snapshot, p rpc.ShareProjectParams) (rpc.ShareProjectResult, error) {
	t, err := resolveTarget(snap, p.Target)
	if err != nil {
		return rpc.ShareProjectResult{}, err
	}
	proj, perr := m.resolveProject(ctx, snap, t)
	if perr != nil {
		// Not a failure of this call: the answer is "no, and here is why".
		return rpc.ShareProjectResult{Available: false, Reason: reasonOf(perr)}, nil
	}
	if !proj.Other() {
		return rpc.ShareProjectResult{
			Available: false,
			Group:     proj.Group,
			Reason:    "this project has nothing else that a share could carry",
		}, nil
	}
	return rpc.ShareProjectResult{
		Available: true,
		Group:     proj.Group,
		Entry:     proj.Entry,
		Services:  proj.describe(),
	}, nil
}

// reasonOf is the sentence out of an rpc error, without the machinery.
func reasonOf(err error) string {
	var re *rpc.Error
	if errors.As(err, &re) {
		if re.Data.Detail != "" {
			return re.Data.Detail
		}
	}
	return err.Error()
}
