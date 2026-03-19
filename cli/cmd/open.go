package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/spf13/cobra"
)

var openCmd = &cobra.Command{
	Use:               "open <port>",
	Short:             "Open localhost:port in the default browser",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePort,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid port: %s", args[0])
		}

		url := fmt.Sprintf("http://localhost:%d", port)

		var openCmd string
		switch runtime.GOOS {
		case "darwin":
			openCmd = "open"
		case "linux":
			openCmd = "xdg-open"
		default:
			return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
		}

		fmt.Printf("Opening %s\n", display.Underline(url))
		return exec.Command(openCmd, url).Start()
	},
}

func init() {
	rootCmd.AddCommand(openCmd)
}
