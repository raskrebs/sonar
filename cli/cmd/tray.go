package cmd

import (
	"github.com/raskrebs/sonar/internal/tray"
	"github.com/spf13/cobra"
)

var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Launch sonar in the system tray",
	Long:  "Start a persistent system tray icon that shows active ports and allows quick access.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return tray.Run()
	},
}

func init() {
	rootCmd.AddCommand(trayCmd)
}
