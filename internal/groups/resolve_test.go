package groups

import (
	"path/filepath"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

type fakePins map[int]string

func (f fakePins) Group(p state.Port) (string, bool) {
	g, ok := f[p.Port]
	return g, ok
}

type fakeRun struct{ group, name string }
type fakeRuns map[int]fakeRun

func (f fakeRuns) Run(p state.Port) (state.Run, bool) {
	r, ok := f[p.Port]
	return state.Run{Group: r.group, Name: r.name}, ok
}

// resolveFixture builds a repo, a linked worktree, a Compose project inside the
// repo and a Compose project outside any repo, and returns the paths.
type fixture struct {
	repo       string // a clone with .git
	repoSub    string // a directory inside the clone
	worktree   string // a linked worktree of the clone
	composeIn  string // compose working_dir inside the clone
	composeOut string // compose working_dir outside any repo
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := tempTree(t)
	repo := mkdir(t, base, "sonar")
	mkdir(t, repo, ".git")
	f := fixture{
		repo:       repo,
		repoSub:    mkdir(t, repo, "backend"),
		worktree:   mkdir(t, base, "wt", "feature-x"),
		composeIn:  mkdir(t, repo, "deploy"),
		composeOut: mkdir(t, base, "loose"),
	}
	writeFile(t, filepath.Join(f.worktree, ".git"),
		"gitdir: "+filepath.Join(repo, ".git", "worktrees", "feature-x")+"\n")
	return f
}

func nativePort(port int, cwd string) state.Port {
	return state.Port{Port: port, BindAddress: "127.0.0.1", Cwd: cwd, Process: "python3"}
}

func composePort(port int, project, service string) state.Port {
	return state.Port{
		Port: port, BindAddress: "0.0.0.0", Process: "com.docke",
		Type:   state.TypeDocker,
		Docker: &state.Docker{Container: project + "-" + service + "-1", ComposeProject: project, ComposeService: service},
	}
}

func TestResolvePrecedence(t *testing.T) {
	f := newFixture(t)

	// A config at the repo root claiming 8000 as its api service, plus 9229 as
	// a bare extra port.
	writeFile(t, filepath.Join(f.repo, ConfigName),
		"name: sonar-cfg\nservices:\n  - name: api\n    port: 8000\nports: [9229]\n")

	index := NewIndex()
	index.Observe(f.repoSub)
	index.AddComposeProject("deploy-stack", f.composeIn)
	index.AddComposeProject("loose-stack", f.composeOut)

	pins := fakePins{7000: "pinned"}
	runs := fakeRuns{7100: {group: "started", name: "web"}, 7200: {group: "", name: "tagged-only"}}

	tests := []struct {
		name       string
		port       state.Port
		wantGroup  string
		wantSource state.GroupSource
		wantRoot   string
	}{
		{
			name:       "manual pin beats everything",
			port:       nativePort(7000, f.repoSub),
			wantGroup:  "pinned",
			wantSource: state.SourceManual,
			wantRoot:   f.repo,
		},
		{
			name:       "a sonar start run beats the config",
			port:       nativePort(7100, f.repoSub),
			wantGroup:  "started",
			wantSource: state.SourceStart,
			wantRoot:   f.repo,
		},
		{
			name:       "a run without a group falls through to the config",
			port:       nativePort(7200, f.repoSub),
			wantGroup:  "sonar-cfg",
			wantSource: state.SourceFile,
			wantRoot:   f.repo,
		},
		{
			name:       "a config service port claims the port",
			port:       nativePort(8000, f.repoSub),
			wantGroup:  "sonar-cfg",
			wantSource: state.SourceFile,
			wantRoot:   f.repo,
		},
		{
			name:       "a config ports entry claims the port",
			port:       nativePort(9229, f.repo),
			wantGroup:  "sonar-cfg",
			wantSource: state.SourceFile,
			wantRoot:   f.repo,
		},
		{
			name:       "a compose project inside the repo merges into it",
			port:       composePort(5432, "deploy-stack", "db"),
			wantGroup:  "sonar-cfg",
			wantSource: state.SourceFile,
			wantRoot:   f.repo,
		},
		{
			name:       "a compose project outside any repo keeps its own name",
			port:       composePort(5433, "loose-stack", "db"),
			wantGroup:  "loose-stack",
			wantSource: state.SourceAuto,
			wantRoot:   "",
		},
		{
			name:       "a compose project with no known directory keeps its name",
			port:       composePort(5434, "unknown-stack", "db"),
			wantGroup:  "unknown-stack",
			wantSource: state.SourceAuto,
			wantRoot:   "",
		},
		{
			// The project half is the main checkout's name, and the main
			// checkout's .sonar.yaml names it (step 5A.6).
			name:       "a worktree is its own group",
			port:       nativePort(8100, f.worktree),
			wantGroup:  "sonar-cfg@feature-x",
			wantSource: state.SourceAuto,
			wantRoot:   f.worktree,
		},
		{
			name:      "a process with no cwd and no docker is ungrouped",
			port:      nativePort(8200, ""),
			wantGroup: "",
		},
		{
			name:      "a process outside any repo is ungrouped",
			port:      nativePort(8300, f.composeOut),
			wantGroup: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve([]state.Port{tt.port}, pins, runs, index)[0]
			gotGroup := deref(got.Group)
			if gotGroup != tt.wantGroup {
				t.Fatalf("group = %q, want %q", gotGroup, tt.wantGroup)
			}
			gotSource := ""
			if got.GroupSource != nil {
				gotSource = string(*got.GroupSource)
			}
			if gotSource != string(tt.wantSource) {
				t.Errorf("group_source = %q, want %q", gotSource, tt.wantSource)
			}
			if deref(got.ProjectRoot) != tt.wantRoot {
				t.Errorf("project_root = %q, want %q", deref(got.ProjectRoot), tt.wantRoot)
			}
		})
	}
}

func TestResolveGitRootWithoutConfig(t *testing.T) {
	f := newFixture(t)
	index := NewIndex()
	index.Observe(f.repoSub)

	got := Resolve([]state.Port{nativePort(8123, f.repoSub)}, NoPins{}, NoRuns{}, index)[0]
	if deref(got.Group) != "sonar" {
		t.Fatalf("group = %q, want the repo directory name", deref(got.Group))
	}
	if got.GroupSource == nil || *got.GroupSource != state.SourceAuto {
		t.Fatalf("group_source = %v, want auto", got.GroupSource)
	}
}

func TestResolveLeavesInputUntouched(t *testing.T) {
	f := newFixture(t)
	in := []state.Port{nativePort(8123, f.repoSub)}
	_ = Resolve(in, NoPins{}, NoRuns{}, NewIndex())
	if in[0].Group != nil || in[0].ProjectRoot != nil {
		t.Fatal("Resolve mutated its input")
	}
}

func TestResolveTolerantOfNilInputs(t *testing.T) {
	got := Resolve([]state.Port{nativePort(8123, "")}, nil, nil, nil)
	if len(got) != 1 || got[0].Group != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestMatchKeys(t *testing.T) {
	root := "/code/sonar"
	p := state.Port{
		Port: 8000, ProjectRoot: &root,
		Run:    &state.Run{Name: "api"},
		Docker: &state.Docker{Container: "sonar-db-1", ComposeProject: "sonar", ComposeService: "db"},
	}
	got := MatchKeys(p)
	want := []string{"run:-/api", "docker:sonar/db", "cwd:/code/sonar:8000", "port:8000"}
	if len(got) != len(want) {
		t.Fatalf("MatchKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MatchKeys = %v, want %v", got, want)
		}
	}

	bare := MatchKeys(state.Port{Port: 3000})
	if len(bare) != 1 || bare[0] != "port:3000" {
		t.Fatalf("MatchKeys(bare) = %v", bare)
	}
}
