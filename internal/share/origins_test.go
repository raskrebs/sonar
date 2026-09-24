package share

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

// A project whose config is on disk, because that is where the environment
// lives — the snapshot deliberately does not carry it.
func configuredGroup(t *testing.T, yaml string) state.Group {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sonar.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return state.Group{Host: state.LocalhostName, Name: "acme", ConfigPath: &path}
}

// The failure this exists to prevent: sign-in returned 400 "invalid
// redirect_uri" because the share's address was not on the backend's list,
// and nothing in the error said which list.
func TestAListThatNamesOnlyLocalhostIsNamed(t *testing.T) {
	g := configuredGroup(t, `
name: acme
services:
  - name: backend
    cmd: run
    env:
      BACKEND_CORS_ORIGINS: http://localhost:5173,http://localhost
      SECRET_KEY: hunter2-do-not-print-me
  - name: frontend
    cmd: run
`)
	note := originNote(g, "https://k4m7q2x9rt8v3wbn.sonarpreview.net")
	if note == "" {
		t.Fatal("a CORS list of only localhost produced no advice")
	}
	for _, want := range []string{"backend", "BACKEND_CORS_ORIGINS",
		"https://k4m7q2x9rt8v3wbn.sonarpreview.net"} {
		if !strings.Contains(note, want) {
			t.Errorf("the advice does not name %q:\n%s", want, note)
		}
	}
	// Names only, never values. The environment holds secrets, which is why
	// this reads the file rather than the snapshot.
	if strings.Contains(note, "hunter2") {
		t.Fatalf("the advice printed an environment value:\n%s", note)
	}
}

// A list that already names a real host has been thought about. Saying
// anything about it spends the one line this gets on a false alarm.
func TestAListThatAlreadyNamesARealHostIsLeftAlone(t *testing.T) {
	g := configuredGroup(t, `
name: acme
services:
  - name: backend
    cmd: run
    env:
      BACKEND_CORS_ORIGINS: http://localhost:5173,https://app.acme.com
`)
	if note := originNote(g, "https://x.sonarpreview.net"); note != "" {
		t.Errorf("advised on a list that already names a real host:\n%s", note)
	}
}

func TestWhichVariablesLookLikeAnOriginList(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"BACKEND_CORS_ORIGINS", true},
		{"CORS_ALLOWED_ORIGINS", true},
		{"ALLOWED_HOSTS", true}, // Django
		{"FRONTEND_HOST", true}, // the redirect target
		{"VITE_PUBLIC_URL", true},
		{"NEXT_PUBLIC_SITE_URL", true},
		// Not lists of origins, and a false alarm on any of them would make
		// the advice worth ignoring.
		{"DATABASE_URL", false},
		{"PORT", false},
		{"SECRET_KEY", false},
		{"REDIS_HOST", false},
		{"VITE_API_URL", false},
	} {
		if got := looksLikeOriginList(tc.name); got != tc.want {
			t.Errorf("looksLikeOriginList(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWhichValuesNameOnlyLocalhost(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"http://localhost:5173", true},
		{"http://localhost:5173,http://localhost,http://127.0.0.1:3000", true},
		{"http://localhost:${frontend.port}", true},
		{"${url}", true},
		{"http://localhost:5173,https://app.acme.com", false},
		{"https://app.acme.com", false},
		{"", false},
	} {
		if got := namesOnlyLocalhost(tc.value); got != tc.want {
			t.Errorf("namesOnlyLocalhost(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// Nothing to say is said in no words at all.
func TestNoAdviceWhenThereIsNothingToAdvise(t *testing.T) {
	g := configuredGroup(t, "name: acme\nservices:\n  - name: web\n    cmd: run\n")
	if note := originNote(g, "https://x.sonarpreview.net"); note != "" {
		t.Errorf("advised a project with no such variable:\n%s", note)
	}
	// And a project with no committed config has nothing to read.
	if note := originNote(state.Group{Name: "loose"}, "https://x.sonarpreview.net"); note != "" {
		t.Errorf("advised a project with no config:\n%s", note)
	}
}
