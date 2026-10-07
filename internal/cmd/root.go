package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

// loadedConfig holds the parsed config for the duration of a command run.
// Populated by PersistentPreRun; never nil after Execute starts.
var loadedConfig = &config.Config{}

const banner = `
  ██████   ███   ██████   ██   ██████
  ██          ██     ██ ██  ██     ██
  ██     ██   ██ ██  ██ ██  ██     ██
      ██ ██   ██ ██  ██ ██  ██ ████
      ██ ██      ██  ██ ██  ██ ██  ██
  ██████   ███   ██  ██ ██  ██ ██  ██
`

var rootCmd = &cobra.Command{
	Use:   "sonar",
	Short: "Detect services listening on localhost ports",
	Long:  display.Cyan(banner) + "\n  " + display.Dim("Detect and manage services listening on localhost ports."),
	// No RunE — bare `sonar` shows help
}

func init() {
	rootCmd.PersistentFlags().Bool("no-color", false, "Disable colored output")
	rootCmd.PersistentFlags().BoolVar(&noDaemonFlag, "no-daemon", false,
		"Never talk to the sonar daemon; scan directly instead")
	// Execute prints the error, once. Left to cobra it is printed twice — by
	// cobra and then by Execute — and an errSilent comes out as a bare "Error:".
	rootCmd.SilenceErrors = true
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		// Reaching here means the flags and arguments parsed, so whatever fails
		// from now on is the work failing, not the invocation: the usage text
		// under it would only bury the message that says what went wrong.
		cmd.SilenceUsage = true

		cfg, warnings := config.Load()
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, w)
		}
		loadedConfig = cfg

		// color: false in config disables ANSI; an explicit --no-color flag
		// also wins. We never force color ON (would corrupt piped output).
		if cfg.Color != nil && !*cfg.Color {
			display.NoColor = true
		}
		if nc, _ := cmd.Flags().GetBool("no-color"); nc {
			display.NoColor = true
		}

		ports.RegisterServices(cfg.Services)
	}
}

// errSilent asks for a non-zero exit without a further message: the command
// has already reported the failure itself, as `--json` output does.
var errSilent = errors.New("")

func Execute() {
	cmd, err := rootCmd.ExecuteC()
	if err == nil {
		return
	}
	if msg := errorLine(cmd, err); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(1)
}

// errorLine is what a failed command prints, or "" when it has already said
// everything itself. A command that sets SilenceErrors writes messages meant
// to be read as they are, several lines of them; every other error gets the
// "Error:" a reader scans for.
func errorLine(cmd *cobra.Command, err error) string {
	switch {
	case errors.Is(err, errSilent):
		return ""
	case cmd != nil && cmd != rootCmd && cmd.SilenceErrors:
		return err.Error()
	default:
		return "Error: " + err.Error()
	}
}
