package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/spf13/cobra"
)

// execute runs the real root command the way a user would, with a temporary
// HOME so nothing touches the developer's config, and returns what landed on
// stdout and stderr.
func execute(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", home)
	t.Setenv("SONAR_SOCKET", filepath.Join(home, "daemon.sock"))
	t.Setenv("SONAR_DB", filepath.Join(home, "sonar.db"))
	t.Setenv(HintEnv, "")

	resetHints()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// countLines counts the stderr lines carrying a migration notice.
func countNotices(stderr string) int {
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, hintPrefix) {
			n++
		}
	}
	return n
}

func TestRunAliasStillWorksAndNoticesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /usr/bin/true to spawn")
	}
	_, stderr, err := execute(t, "run", "--tag", "demo", "--", "true")
	if err != nil {
		t.Fatalf("sonar run --tag demo -- true: %v", err)
	}
	if n := countNotices(stderr); n != 1 {
		t.Fatalf("want exactly one notice, got %d in:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "sonar start --group demo -- true") {
		t.Errorf("notice does not show the replacement command:\n%s", stderr)
	}
}

func TestRunsAliasStillWorksAndNotices(t *testing.T) {
	runsJSONFlag = false
	stdout, stderr, err := execute(t, "runs")
	if err != nil {
		t.Fatalf("sonar runs: %v", err)
	}
	if countNotices(stderr) != 1 {
		t.Fatalf("want one notice, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "sonar start --list") {
		t.Errorf("notice does not point at start --list:\n%s", stderr)
	}
	if strings.Contains(stdout, hintPrefix) {
		t.Errorf("notice leaked onto stdout:\n%s", stdout)
	}
}

func TestRunsAliasIsSilentWithJSON(t *testing.T) {
	defer func() { runsJSONFlag = false }()
	_, stderr, err := execute(t, "runs", "--json")
	if err != nil {
		t.Fatalf("sonar runs --json: %v", err)
	}
	if countNotices(stderr) != 0 {
		t.Fatalf("--json must not print a notice, got:\n%s", stderr)
	}
}

func TestProfileAliasNoticesAndExports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".config", "sonar", "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const prof = "name: my-app\nports:\n  - port: 8000\n    name: api\n    health: true\n  - port: 5173\n    name: frontend\n"
	if err := os.WriteFile(filepath.Join(dir, "my-app.yaml"), []byte(prof), 0o644); err != nil {
		t.Fatal(err)
	}

	// The old command still works and says what replaces it.
	resetHints()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs([]string{"profile", "show", "my-app"})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("sonar profile show: %v", err)
	}
	if countNotices(errOut.String()) != 1 {
		t.Fatalf("want one notice, got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "sonar profile export my-app") {
		t.Errorf("notice does not offer the export command:\n%s", errOut.String())
	}
}

// The environment switch has to work through a real invocation, not only
// through the helper: a script that sets it once should never see a notice.
func TestNoHintsEnvSilencesARealInvocation(t *testing.T) {
	runsJSONFlag = false
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(HintEnv, "1")
	resetHints()

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs([]string{"runs"})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("sonar runs: %v", err)
	}
	if countNotices(errOut.String()) != 0 {
		t.Fatalf("SONAR_NO_HINTS=1 must silence the notice, got:\n%s", errOut.String())
	}
}

func TestNoHintsEnvSilencesTheNotice(t *testing.T) {
	t.Setenv(HintEnv, "1")
	resetHints()
	var errOut bytes.Buffer
	c := &cobra.Command{Use: "x"}
	c.SetErr(&errOut)
	Hint(c, HintRunsToStartList())
	if errOut.Len() != 0 {
		t.Fatalf("SONAR_NO_HINTS=1 must silence the notice, got %q", errOut.String())
	}
}

func TestNoHintsEnvOffLeavesNoticesOn(t *testing.T) {
	for _, v := range []string{"", "0", "false"} {
		t.Setenv(HintEnv, v)
		if HintsDisabled() {
			t.Errorf("SONAR_NO_HINTS=%q should not disable hints", v)
		}
	}
	for _, v := range []string{"1", "true", "yes"} {
		t.Setenv(HintEnv, v)
		if !HintsDisabled() {
			t.Errorf("SONAR_NO_HINTS=%q should disable hints", v)
		}
	}
}

func TestHintPrintsAtMostOncePerInvocation(t *testing.T) {
	t.Setenv(HintEnv, "")
	resetHints()
	var errOut bytes.Buffer
	c := &cobra.Command{Use: "x"}
	c.SetErr(&errOut)
	Hint(c, HintRunsToStartList())
	Hint(c, HintDownToKill("my-app"))
	if n := countNotices(errOut.String()); n != 1 {
		t.Fatalf("two hints in one invocation printed %d lines:\n%s", n, errOut.String())
	}
}

func TestHintIsSilentWhenTheCommandPrintsJSON(t *testing.T) {
	t.Setenv(HintEnv, "")
	resetHints()
	var errOut bytes.Buffer
	c := &cobra.Command{Use: "x"}
	c.Flags().Bool("json", false, "")
	c.SetErr(&errOut)
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	Hint(c, HintRunsToStartList())
	if errOut.Len() != 0 {
		t.Fatalf("--json must silence the notice, got %q", errOut.String())
	}
}

func TestNoticeTexts(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"run", HintRunToStart("api", []string{"npm", "run", "dev"}),
			"run is now start — use: sonar start --group api -- npm run dev"},
		{"runs", HintRunsToStartList(), "runs is now start --list — use: sonar start --list"},
		{"down", HintDownToKill("my-app"), "down is now kill -g — use: sonar kill -g my-app"},
		{"kill-all", HintKillAllToKill("docker", "", true, false),
			"kill-all is now kill --all — use: sonar kill --all --filter docker --yes"},
		{"tag", HintTagToGroup("list", "my-app"),
			"--tag is now --group — use: sonar list --group my-app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("notice = %q, want %q", tt.got, tt.want)
			}
		})
	}
	for _, s := range []string{
		HintProfileToConfig("my-app"), HintUpProfile("my-app"),
	} {
		if !strings.Contains(s, groups.ConfigName) {
			t.Errorf("notice %q should name %s", s, groups.ConfigName)
		}
		if strings.Contains(s, "\n") {
			t.Errorf("notice %q must be a single line", s)
		}
	}
}
