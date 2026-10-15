package runsreg

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/state"
)

// testRegistry is a registry with no process table and no runs.json behind it.
func testRegistry(alive ...int) *Registry {
	live := map[int]bool{}
	for _, pid := range alive {
		live[pid] = true
	}
	r := New()
	r.Mirror = false
	r.Alive = func(pid int) bool { return live[pid] }
	r.Parents = func() map[int]int { return nil }
	return r
}

func TestRegisterListAndUnregister(t *testing.T) {
	r := testRegistry(100, 200)
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	r.Register(Record{ID: "b", PID: 200, Group: "g", Name: "api", StartedAt: base.Add(time.Minute)})
	r.Register(Record{ID: "a", PID: 100, Group: "g", Name: "web", StartedAt: base})

	got := r.List()
	if len(got) != 2 {
		t.Fatalf("List = %d runs, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("List is not oldest first: %v", []string{got[0].ID, got[1].ID})
	}

	if !r.Unregister(100) {
		t.Fatal("Unregister(100) reported nothing to remove")
	}
	if r.Unregister(100) {
		t.Fatal("Unregister(100) removed the same run twice")
	}
	if len(r.List()) != 1 {
		t.Fatalf("List after unregister = %v", r.List())
	}
}

func TestRegisterKeepsTheIDOfAReregisteredPID(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "keep", PID: 100, Group: "g", Name: "web"})
	rec := r.Register(Record{PID: 100, Group: "g", Name: "web"})
	if rec.ID != "keep" {
		t.Fatalf("id = %q, want keep", rec.ID)
	}
}

func TestPruneDropsDeadRuns(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", PID: 100, Group: "g", Name: "web"})
	r.Register(Record{ID: "b", PID: 999, Group: "g", Name: "gone"})

	r.Prune()
	got := r.List()
	if len(got) != 1 || got[0].PID != 100 {
		t.Fatalf("Prune left %v, want only pid 100", got)
	}
}

func TestServiceLiveAsksOnlyAboutThatServicesRuns(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", PID: 100, Group: "g", Name: "worker"})
	r.Register(Record{ID: "b", PID: 999, Group: "g", Name: "gone"})

	if !r.ServiceLive("g", "worker") {
		t.Fatal("a service whose run is alive should be live")
	}
	if r.ServiceLive("g", "gone") {
		t.Fatal("a service whose run has exited should not be live")
	}
	if r.ServiceLive("other", "worker") {
		t.Fatal("a run in another group should not make the service live")
	}
}

func TestRunAttributesAPortByItsPPIDAncestry(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", PID: 100, Group: "itest", Name: "web"})
	// sonar start (100) -> npm (200) -> node (300) -> esbuild (400)
	r.Parents = func() map[int]int { return map[int]int{400: 300, 300: 200, 200: 100} }

	run, ok := r.Run(state.Port{PID: 400, PPID: 300})
	if !ok || run.Group != "itest" || run.Name != "web" || run.RootPID != 100 {
		t.Fatalf("Run(descendant) = %+v/%v, want itest/web rooted at 100", run, ok)
	}

	if _, ok := r.Run(state.Port{PID: 777, PPID: 1}); ok {
		t.Fatal("an unrelated listener was attributed to a run")
	}
}

// TestRunAttributesTheLinuxScannerShape is the shape a Linux scan hands the
// resolver: `ss -tlnp` reports the listening pid and nothing else, so the row
// arrives with PPID 0 and no run of its own. Attribution then has only the
// process table to work with, and it has to reach the `sonar start` that owns
// the listener through it.
func TestRunAttributesTheLinuxScannerShape(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", PID: 100, Group: "itest", Name: "web"})
	// sonar start (100) -> the listener it spawned (300). ss gave no ppid.
	r.Parents = func() map[int]int { return map[int]int{300: 100, 100: 42} }

	run, ok := r.Run(state.Port{PID: 300, PPID: 0})
	if !ok {
		t.Fatal("a listener spawned by a run was not attributed to it")
	}
	if run.ID != "a" || run.Group != "itest" || run.Name != "web" || run.RootPID != 100 {
		t.Fatalf("Run = %+v, want a/itest/web rooted at 100", run)
	}
}

// TestRunAttributesTheRegisteredPIDItself covers `sonar start` in the
// foreground: the run is registered under the pid of the child it spawned, and
// that child is the process holding the socket, so no walk is needed and the
// answer must not depend on a process table being readable at all.
func TestRunAttributesTheRegisteredPIDItself(t *testing.T) {
	r := testRegistry(300)
	r.Register(Record{ID: "a", PID: 300, Group: "itest", Name: "web"})
	r.Parents = func() map[int]int { t.Fatal("the process table should not be needed"); return nil }

	run, ok := r.Run(state.Port{PID: 300})
	if !ok || run.ID != "a" || run.Name != "web" || run.RootPID != 300 {
		t.Fatalf("Run = %+v/%v, want the registered run", run, ok)
	}
}

func TestRunUsesTheDirectParentBeforeTheProcessTable(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", PID: 100, Group: "itest", Name: "web"})
	r.Parents = func() map[int]int { t.Fatal("the process table should not be needed"); return nil }

	if run, ok := r.Run(state.Port{PID: 200, PPID: 100}); !ok || run.Group != "itest" {
		t.Fatalf("Run(child) = %+v/%v", run, ok)
	}
}

func TestRunFallsBackToTheScannersOwnAttribution(t *testing.T) {
	r := testRegistry()
	run, ok := r.Run(state.Port{
		PID: 400,
		Run: &state.Run{ID: "x", Group: "from-file", Name: "web", RootPID: 100},
	})
	if !ok || run.Group != "from-file" || run.Name != "web" {
		t.Fatalf("Run = %+v/%v", run, ok)
	}
}

func TestImportLegacyTakesOverRunsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	live := os.Getpid()
	if err := runs.Add(runs.Entry{PID: live, Tag: "legacy", ID: "old-1", Cmd: "npm run dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runs.Path()); err != nil {
		t.Fatalf("runs.json was not written: %v", err)
	}

	r := New()
	r.Mirror = false
	if n := r.ImportLegacy(); n != 1 {
		t.Fatalf("ImportLegacy = %d, want 1", n)
	}
	if _, err := os.Stat(runs.Path()); !os.IsNotExist(err) {
		t.Fatalf("runs.json survived the import: %v", err)
	}

	got := r.List()
	if len(got) != 1 {
		t.Fatalf("List = %v", got)
	}
	// A `sonar run --tag legacy` entry carries no group or name of its own, so
	// the tag stands for both (the documented alias).
	if got[0].Group != "legacy" || got[0].Name != "legacy" || got[0].ID != "old-1" {
		t.Fatalf("imported record = %+v", got[0])
	}
}

func TestImportLegacyRewritesTheFileItOwns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	live := os.Getpid()
	if err := runs.Add(runs.Entry{PID: live, Tag: "legacy", ID: "old-1"}); err != nil {
		t.Fatal(err)
	}

	r := New() // mirroring on: the daemon owns the file from here
	if n := r.ImportLegacy(); n != 1 {
		t.Fatalf("ImportLegacy = %d, want 1", n)
	}
	data, err := os.ReadFile(runs.Path())
	if err != nil {
		t.Fatalf("the daemon did not rewrite runs.json: %v", err)
	}
	var file struct {
		Runs map[string]runs.Entry `json:"runs"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	entry, ok := file.Runs[itoa(live)]
	if !ok {
		t.Fatalf("runs.json = %s", data)
	}
	if entry.Group != "legacy" || entry.Name != "legacy" {
		t.Fatalf("mirrored entry = %+v", entry)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
