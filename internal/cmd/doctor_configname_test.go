package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/raskrebs/sonar/internal/groups"
)

// TestRenameLegacyConfigPlain: outside git the fix is a plain rename.
func TestRenameLegacyConfigPlain(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, groups.LegacyConfigName)
	if err := os.WriteFile(old, []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renameLegacyConfig(dir, groups.FilesIn(dir)); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("%s is still there", old)
	}
	if data, err := os.ReadFile(filepath.Join(dir, groups.ConfigName)); err != nil || string(data) != "name: demo\n" {
		t.Errorf("%s = %q, %v", groups.ConfigName, data, err)
	}
}

// TestRenameLegacyConfigTracked: a tracked file is moved with git mv, so the
// rename is staged.
func TestRenameLegacyConfigTracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitIn("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, groups.LegacyConfigName), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn("add", groups.LegacyConfigName)

	if _, err := renameLegacyConfig(dir, groups.FilesIn(dir)); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !tracked(dir, groups.ConfigName) {
		t.Errorf("%s is not staged after the fix", groups.ConfigName)
	}
	if tracked(dir, groups.LegacyConfigName) {
		t.Errorf("%s is still tracked after the fix", groups.LegacyConfigName)
	}
}

// TestRenameLegacyConfigRefusesTwoFiles: with both names present the fix
// touches nothing — one of them would be lost.
func TestRenameLegacyConfigRefusesTwoFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{groups.ConfigName: "name: new\n", groups.LegacyConfigName: "name: old\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := renameLegacyConfig(dir, groups.FilesIn(dir)); err == nil {
		t.Fatal("renamed with two config files present")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, groups.ConfigName)); string(data) != "name: new\n" {
		t.Errorf("%s changed: %q", groups.ConfigName, data)
	}
}
