//go:build !windows

package ports

import (
	"fmt"
	"syscall"
)

// KillPID sends a signal to a process by PID.
func KillPID(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}

	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("failed to kill PID %d: %w", pid, err)
	}

	return nil
}
