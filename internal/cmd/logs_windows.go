//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/raskrebs/sonar/internal/ports"
)

// execDockerLogs runs docker logs for a container.
func execDockerLogs(container string) error {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker not found in PATH")
	}

	args := []string{"logs"}
	if logsFollow {
		args = append(args, "-f")
	}
	args = append(args, container)

	cmd := exec.Command(dockerPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// tailLogSources is not supported on Windows.
func tailLogSources(_ []ports.LogSource) error {
	return fmt.Errorf("log viewing is not supported on Windows for non-Docker processes")
}

// execLogStream is not supported on Windows.
func execLogStream(_ int) error {
	return fmt.Errorf("log stream is not supported on Windows")
}

// tailProcFD is not supported on Windows.
func tailProcFD(_ int) error {
	return fmt.Errorf("log viewing is not supported on Windows for non-Docker processes")
}
