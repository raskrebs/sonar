//go:build windows

package ports

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// KillPID terminates a process by PID using taskkill.
func KillPID(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}

	out, err := exec.Command("taskkill", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to kill PID %d: %s", pid, strings.TrimSpace(string(out)))
	}

	return nil
}
