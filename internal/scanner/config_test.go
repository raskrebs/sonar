package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
)

// TestScanPicksUpAnEditedConfig: the daemon's index is long-lived, so a file
// edited underneath it would otherwise stay as it was read. One stat per known
// file on each tick is what keeps it honest, with no watcher and no new
// dependency.
func TestScanPicksUpAnEditedConfig(t *testing.T) {
	dir := resolved(t, t.TempDir())
	writeConfig(t, dir, "name: before\nservices:\n  - name: api\n    port: 8000\n")

	l := loopWith(nil, ports.ListeningPort{
		Port: 8000, PID: 42, Process: "python3", Cwd: dir,
	})
	snap, err := l.Snapshot(Include{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Groups) != 1 || snap.Groups[0].Name != "before" {
		t.Fatalf("groups = %+v", snap.Groups)
	}

	time.Sleep(10 * time.Millisecond)
	writeConfig(t, dir, "name: after\nservices:\n  - name: api\n    port: 8000\n    description: edited\n")
	l.Invalidate()

	snap, err = l.Snapshot(Include{})
	if err != nil {
		t.Fatalf("Snapshot after the edit: %v", err)
	}
	if len(snap.Groups) != 1 || snap.Groups[0].Name != "after" {
		t.Fatalf("groups after the edit = %+v", snap.Groups)
	}
	svc := snap.Groups[0].Services[0]
	if svc.Description == nil || *svc.Description != "edited" {
		t.Fatalf("the edited metadata did not reach the snapshot: %+v", svc)
	}
}

// TestReloadConfigsPicksUpANewFile is `groups.reload` at the scanner level: a
// root the store remembers is re-read even though no process has been seen in
// it yet. The root is added after the first scan: one that holds no config
// when the daemon starts is forgotten as dead.
func TestReloadConfigsPicksUpANewFile(t *testing.T) {
	st := openStore(t)
	dir := resolved(t, t.TempDir())

	l := loopWith(st)
	if _, err := l.Snapshot(Include{}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := st.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	if got := len(l.Configs()); got != 0 {
		t.Fatalf("configs = %d, want none before the file exists", got)
	}

	if err := os.WriteFile(filepath.Join(dir, ".sonar.yaml"),
		[]byte("name: fresh\nservices:\n  - name: api\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, bad := l.ReloadConfigs()
	if loaded != 1 || len(bad) != 0 {
		t.Fatalf("ReloadConfigs = %d, %v", loaded, bad)
	}
	if cfg, ok := l.ConfigNamed("fresh"); !ok || cfg.Services[0].Name != "api" {
		t.Fatalf("ConfigNamed(fresh) = %+v, %v", cfg, ok)
	}
	if _, ok := l.ConfigAt(filepath.Join(dir, ".sonar.yaml")); !ok {
		t.Error("ConfigAt does not find the file that was just loaded")
	}
}
