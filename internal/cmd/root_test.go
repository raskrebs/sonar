package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// A failed command prints its error once. What it prints depends on who has
// already spoken: nothing for errSilent, the message as written for a command
// that formats its own, and "Error: …" for everything else.
func TestErrorLine(t *testing.T) {
	plain := &cobra.Command{Use: "plain"}
	own := &cobra.Command{Use: "own", SilenceErrors: true}

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		err  error
		want string
	}{
		{"an ordinary error", plain, errors.New("no such group"), "Error: no such group"},
		{"a command that words its own", own, errors.New("which port?"), "which port?"},
		{"already reported", plain, errSilent, ""},
		{"already reported, wrapped", plain, errors.Join(errSilent), ""},
		// The root silences cobra's printing for everyone; that is not it
		// asking for its own errors to go out bare.
		{"the root command", rootCmd, errors.New("unknown command"), "Error: unknown command"},
	} {
		if got := errorLine(tc.cmd, tc.err); got != tc.want {
			t.Errorf("%s: errorLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}
