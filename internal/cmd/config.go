package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/spf13/cobra"
)

var configForce bool

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage the sonar config file",
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file path",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(config.Path())
		return nil
	},
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starter config file",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := runConfigInit(configForce); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", config.Path())
		return nil
	},
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open the config file in $EDITOR",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigEdit()
	},
}

func init() {
	configInitCmd.Flags().BoolVar(&configForce, "force", false, "Overwrite an existing config file")
	configCmd.AddCommand(configPathCmd, configInitCmd, configEditCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfigInit(force bool) error {
	return config.WriteTemplate(force)
}

func runConfigEdit() error {
	// Ensure a file exists so the editor opens something real.
	if _, err := os.Stat(config.Path()); os.IsNotExist(err) {
		if err := config.WriteTemplate(false); err != nil {
			return err
		}
	}
	editor := resolveEditor()
	if editor == "" {
		return fmt.Errorf("no editor found: set $EDITOR (or $VISUAL)")
	}
	c := exec.Command(editor, config.Path())
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func resolveEditor() string {
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	if e := os.Getenv("VISUAL"); e != "" {
		return e
	}
	if runtime.GOOS == "windows" {
		return "notepad"
	}
	if _, err := exec.LookPath("vi"); err == nil {
		return "vi"
	}
	return ""
}
