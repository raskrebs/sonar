package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/spf13/cobra"
)

// hints.go holds the migration notices for the commands the group-and-daemon
// model renamed (daemon spec, "Migration and deprecation"). Every alias keeps
// working; each one prints a single line on stderr telling the user what to
// type instead.
//
// The rules, in one place so every alias obeys them:
//
//   - stderr, never stdout, so a piped command's output is untouched;
//   - at most one notice per invocation, however many aliases a command run
//     touches;
//   - silent when the command is producing JSON;
//   - silent when SONAR_NO_HINTS is set, for scripts and CI.

// HintEnv is the environment variable that silences every migration notice.
const HintEnv = "SONAR_NO_HINTS"

// hintPrefix keeps notices recognisable as sonar's own output when they land
// in a log next to the child process's stderr.
const hintPrefix = "sonar: "

// hintOnce enforces "one notice per invocation". It is package state because
// the notice is a property of the run, not of a command object.
var hintOnce sync.Once

// resetHints puts the once back for a test that runs several invocations in
// one process.
func resetHints() { hintOnce = sync.Once{} }

// HintsDisabled reports whether the environment asked for silence.
// SONAR_NO_HINTS=0, =false or empty leave notices on, so `SONAR_NO_HINTS=0` in
// a shell profile does not read as "disabled".
func HintsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(HintEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// Hint prints one migration notice for cmd, unless the invocation is producing
// JSON, the environment silenced hints, or a notice was already printed.
//
// `sonar up` calls it as Hint(cmd, HintUpProfile(name)) when the group it could
// not find is still a profile.
func Hint(cmd *cobra.Command, msg string) {
	if msg == "" || HintsDisabled() || jsonRequested(cmd) {
		return
	}
	hintOnce.Do(func() {
		if cmd != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), hintPrefix+msg)
			return
		}
		fmt.Fprintln(os.Stderr, hintPrefix+msg)
	})
}

// jsonRequested reports whether the command was asked for machine-readable
// output. A notice on stderr would not corrupt the JSON on stdout, but a tool
// reading both streams should not have to filter our prose.
func jsonRequested(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	f := cmd.Flags().Lookup("json")
	if f == nil {
		return false
	}
	return f.Value.String() == "true"
}

// The notice texts. They are functions rather than constants because each one
// echoes the user's own arguments back: a notice that shows the exact command
// to type next is acted on, one that describes a rename is not.

// HintRunToStart is the notice for `sonar run --tag X -- cmd`.
func HintRunToStart(tag string, argv []string) string {
	group := tag
	if group == "" {
		group = "<group>"
	}
	command := strings.Join(argv, " ")
	if command == "" {
		command = "<command>"
	}
	return fmt.Sprintf("run is now start — use: sonar start --group %s -- %s", group, command)
}

// HintRunsToStartList is the notice for `sonar runs`.
func HintRunsToStartList() string {
	return "runs is now start --list — use: sonar start --list"
}

// HintProfileToConfig is the notice every `sonar profile` subcommand prints:
// profiles are a per-machine file, `sonar.yaml` is committed with the project.
func HintProfileToConfig(name string) string {
	if name == "" {
		name = "<name>"
	}
	return fmt.Sprintf(
		"profiles are replaced by %s — use: sonar profile export %s > %s, or sonar init",
		groups.ConfigName, name, groups.ConfigName)
}

// HintUpProfile is the notice `sonar up <profile>` prints when the name it was
// given is not a group but is still a profile. up.go reaches it through Hint,
// so it obeys the same one-line, --json and SONAR_NO_HINTS rules as every other
// notice here.
func HintUpProfile(name string) string {
	if name == "" {
		name = "<name>"
	}
	return fmt.Sprintf(
		"up now starts a group from %s — export the profile first: sonar profile export %s > %s",
		groups.ConfigName, name, groups.ConfigName)
}

// HintDownToKill is the notice for `sonar down <profile>`.
func HintDownToKill(name string) string {
	if name == "" {
		name = "<group>"
	}
	return fmt.Sprintf("down is now kill -g — use: sonar kill -g %s", name)
}

// HintKillAllToKill is the notice for `sonar kill-all`, echoing the flags the
// user passed so the replacement line is copy-pasteable.
func HintKillAllToKill(filter, project string, yes, force bool) string {
	var b strings.Builder
	b.WriteString("kill-all is now kill --all — use: sonar kill --all")
	if filter != "" {
		fmt.Fprintf(&b, " --filter %s", filter)
	}
	if project != "" {
		fmt.Fprintf(&b, " --project %s", project)
	}
	if force {
		b.WriteString(" --force")
	}
	if yes {
		b.WriteString(" --yes")
	}
	return b.String()
}

// HintTagToGroup is the notice for the `--tag` flag wherever it survives as an
// alias of `--group`.
func HintTagToGroup(command, value string) string {
	if value == "" {
		value = "<group>"
	}
	return fmt.Sprintf("--tag is now --group — use: sonar %s --group %s", command, value)
}
