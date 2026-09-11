package groups

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/raskrebs/sonar/internal/ports"
)

// header is written above a generated config so the next reader knows where it
// came from and that it is meant to be edited and committed.
const header = `# ` + ConfigName + ` — sonar group configuration, written by ` + "`sonar init`" + `.
# Edit and commit it: sonar uses it to name this project's group and to know
# which services belong to it. https://github.com/raskrebs/sonar
`

// Propose builds a config for the checkout at root from what is listening
// right now: every port whose process works inside the checkout, plus every
// Compose container started from it. Desktop apps and system ports are left
// out — they are not this project's services.
//
// index may be nil; it is only consulted for Compose working directories.
func Propose(root string, pp []ports.ListeningPort, index *Index) *Config {
	if index == nil {
		index = NewIndex()
	}
	_, worktree, _ := Find(root)
	cfg := &Config{
		Name: GroupName(root, worktree),
		Dir:  root,
		Path: filepath.Join(root, ConfigName),
	}

	candidates := make([]ports.ListeningPort, 0, len(pp))
	for _, p := range pp {
		if p.IsApp {
			continue
		}
		// Well-known ports are the machine's, not the project's.
		if p.Type != ports.PortTypeDocker && ports.ClassifyPort(p.Port) == ports.PortTypeSystem {
			continue
		}
		if dir := portDir(p, index); dir != "" && under(dir, root) {
			candidates = append(candidates, p)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Port < candidates[j].Port })

	used := map[string]bool{}
	for _, p := range candidates {
		if used[serviceKey(p)] {
			continue
		}
		used[serviceKey(p)] = true
		cfg.Services = append(cfg.Services, proposeService(p, root, index, used))
	}
	return cfg
}

// serviceKey collapses the several bind addresses of one listener (IPv4 and
// IPv6, wildcard and loopback) into a single proposed service.
func serviceKey(p ports.ListeningPort) string { return fmt.Sprintf("%d/%d", p.Port, p.PID) }

func proposeService(p ports.ListeningPort, root string, index *Index, used map[string]bool) Service {
	svc := Service{Name: uniqueName(proposeName(p), p.Port, used), Port: p.Port, Cmd: proposeCmd(p)}
	if dir := portDir(p, index); dir != "" && dir != root {
		if rel, err := filepath.Rel(root, dir); err == nil && rel != "." {
			svc.Cwd = filepath.ToSlash(rel)
		}
	}
	return svc
}

// portDir is the directory a port belongs to: the process cwd, the Compose
// project's working directory for a container, or — when neither is at hand —
// the project root the resolver already attributed the port to.
//
// The last arm is what lets the daemon propose from a published snapshot.
// state.Port carries no Compose working directory (the resolver consumes it
// before a row is published), but it does carry project_root, which for a
// Compose container *is* the git root above that working directory. Without it
// a `groups.init` served by the daemon would silently drop every container the
// CLI proposes. It only fires when the two better answers are absent, so the
// direct path is unchanged.
func portDir(p ports.ListeningPort, index *Index) string {
	if p.Cwd != "" {
		return p.Cwd
	}
	if p.DockerComposeProject != "" {
		if dir := index.ComposeDir(p.DockerComposeProject); dir != "" {
			return dir
		}
	}
	return p.ProjectRoot
}

// proposeCmd guesses the command that would bring this service back up.
func proposeCmd(p ports.ListeningPort) string {
	if p.DockerComposeService != "" {
		return "docker compose up " + p.DockerComposeService
	}
	if p.DockerContainer != "" {
		return "docker start " + p.DockerContainer
	}
	return strings.TrimSpace(p.Command)
}

// proposeName picks a proposed service name from what the scanner knows.
func proposeName(p ports.ListeningPort) string {
	if p.DockerComposeService != "" {
		return p.DockerComposeService
	}
	if p.Tag != "" {
		return p.Tag
	}
	return p.DisplayName()
}

// uniqueName sanitises a candidate name into something the config validator
// accepts and that is unique within the file.
func uniqueName(candidate string, port int, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(candidate) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	if name == "" {
		name = fmt.Sprintf("service-%d", port)
	}
	base := name
	for i := 2; used["name:"+name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used["name:"+name] = true
	return name
}

// Marshal renders a config as the bytes to write to disk, with the generated
// header on top.
func Marshal(cfg *Config) ([]byte, error) {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append([]byte(header), body...), nil
}
