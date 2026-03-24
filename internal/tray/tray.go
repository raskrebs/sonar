//go:build !windows

package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// Run starts the system tray application. On macOS it launches the native
// Swift menu bar app (sonar-tray). On other platforms it returns an error.
// When detach is true, the tray app is started in the background and the
// command returns immediately.
func Run(detach bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("system tray is currently only supported on macOS")
	}

	trayBin, err := findTrayBinary()
	if err != nil {
		return err
	}

	cmd := exec.Command(trayBin)

	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start tray: %w", err)
		}
		fmt.Fprintf(os.Stderr, "sonar tray running (pid %d)\n", cmd.Process.Pid)
		return nil
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// findTrayBinary looks for the sonar-tray binary next to the sonar binary,
// then falls back to $PATH.
func findTrayBinary() (string, error) {
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "sonar-tray")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	if path, err := exec.LookPath("sonar-tray"); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("sonar-tray binary not found; it should be installed alongside the sonar binary")
}
