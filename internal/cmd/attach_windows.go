//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

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

	cmd := exec.Command(dockerPath, "exec", "-it", container, shell)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execTCPConnect opens a raw TCP connection to localhost:<port>.
func execTCPConnect(port int) error {
	fmt.Printf("%s %s\n\n",
		display.Dim("Connecting to"),
		display.BoldCyan(fmt.Sprintf("localhost:%d", port)))

	for _, name := range []string{"ncat", "nc"} {
		binPath, err := exec.LookPath(name)
		if err == nil {
			cmd := exec.Command(binPath, "localhost", strconv.Itoa(port))
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	}

	return fmt.Errorf("no TCP client found (install ncat)\n\n%s",
		display.Dim("Alternatively, connect manually:\n"+
			fmt.Sprintf("  ncat localhost %d", port)))
}
