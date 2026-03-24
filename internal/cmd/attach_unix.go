//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/raskrebs/sonar/internal/display"
)

// execDockerShell execs into a Docker container with an interactive shell.
func execDockerShell(container string) error {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker not found in PATH")
	}

	shell := attachShell
	if shell == "" {
		shell = detectContainerShell(container)
	}

	fmt.Printf("%s %s %s\n\n",
		display.Dim("Attaching shell to container"),
		display.Bold(container),
		display.Dim("("+shell+")"))

	args := []string{"docker", "exec", "-it", container, shell}
	return syscall.Exec(dockerPath, args, os.Environ())
}

// execTCPConnect opens a raw TCP connection to localhost:<port>.
func execTCPConnect(port int) error {
	fmt.Printf("%s %s\n\n",
		display.Dim("Connecting to"),
		display.BoldCyan(fmt.Sprintf("localhost:%d", port)))

	// Prefer ncat/nc for raw TCP connections
	for _, name := range []string{"ncat", "nc"} {
		binPath, err := exec.LookPath(name)
		if err == nil {
			args := []string{name, "localhost", strconv.Itoa(port)}
			return syscall.Exec(binPath, args, os.Environ())
		}
	}

	return fmt.Errorf("no TCP client found (install ncat or nc)\n\n%s",
		display.Dim("Alternatively, connect manually:\n"+
			fmt.Sprintf("  nc localhost %d", port)))
}
