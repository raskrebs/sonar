package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current, remote string
		want            bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"dev", "v1.0.0", false},
		{"v1.0.0", "v2.0.0", true},
	}

	for _, tt := range tests {
		if got := IsNewer(tt.current, tt.remote); got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.remote, got, tt.want)
		}
	}
}

// The binary an update replaces is the one running it. Linux refuses to open a
// running executable for writing ("text file busy"), so the check must not ask
// that: the test binary stands in for sonar here, and it is running.
func TestCheckWritableAcceptsTheRunningBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := checkWritable(exe); err != nil {
		t.Fatalf("checkWritable(%s) = %v, want nil for a running binary in a writable directory", exe, err)
	}
}

// What an update needs is the directory: the new binary is renamed into it. A
// read-only file in a writable directory can be replaced, and any file in a
// directory that cannot be written to cannot.
func TestCheckWritableAsksTheDirectoryNotTheFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory permissions do not bind here")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "sonar")
	if err := os.WriteFile(bin, []byte("x"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := checkWritable(bin); err != nil {
		t.Errorf("a read-only binary in a writable directory: %v, want nil", err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := checkWritable(bin); err == nil {
		t.Error("a binary in a read-only directory was accepted")
	}
}
