package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls what the follower wrote until it contains want.
func waitForOutput(t *testing.T, out *lineWriter, buf *bytes.Buffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out.mu.Lock()
		got := buf.String()
		out.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%s", want, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLogFollowerStartsAtTheOffset: what an earlier run wrote is not replayed,
// and a line written in two pieces comes out as one.
func TestLogFollowerStartsAtTheOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.log")
	appendTo(t, path, "an earlier run\n")

	var buf bytes.Buffer
	out := &lineWriter{w: &buf}
	f := newLogFollower(out, "api | ", path, int64(len("an earlier run\n")))
	f.Start()
	defer f.Stop()

	appendTo(t, path, "first\nsecond, written ")
	time.Sleep(3 * followPoll)
	appendTo(t, path, "in two pieces\n")

	got := waitForOutput(t, out, &buf, "api | second, written in two pieces")
	if strings.Contains(got, "earlier run") {
		t.Errorf("replayed an earlier run:\n%s", got)
	}
	if !strings.HasPrefix(got, "api | first\n") {
		t.Errorf("output = %q, want it to start with the first new line", got)
	}
}

// TestLogFollowerStartsOverAfterARotation: an offset past the end of the file
// means the file was replaced, so the follower reads the new one from the top.
func TestLogFollowerStartsOverAfterARotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.log")
	appendTo(t, path, "fresh file\n")

	var buf bytes.Buffer
	out := &lineWriter{w: &buf}
	f := newLogFollower(out, "api | ", path, 10<<20)
	f.Start()
	defer f.Stop()

	waitForOutput(t, out, &buf, "api | fresh file")
}

// TestLogFollowerFlushesOnStop: a last line without a newline still comes out.
func TestLogFollowerFlushesOnStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.log")
	var buf bytes.Buffer
	out := &lineWriter{w: &buf}
	f := newLogFollower(out, "api | ", path, 0)
	f.Start()

	appendTo(t, path, "no newline at the end")
	f.Stop()
	if got := buf.String(); got != "api | no newline at the end\n" {
		t.Errorf("output = %q", got)
	}
}
