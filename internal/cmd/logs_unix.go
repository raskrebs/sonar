//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/ports"
)

// execDockerLogs execs into docker logs for a container.
func execDockerLogs(container string) error {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker not found in PATH")
	}

	args := []string{"docker", "logs"}
	if logsFollow {
		args = append(args, "-f")
	}
	args = append(args, container)

	return syscall.Exec(dockerPath, args, os.Environ())
}

// tailLogSources tails discovered log files.
func tailLogSources(sources []ports.LogSource) error {
	// Print what we found
	for _, s := range sources {
		label := s.Path
		if s.FD != "" {
			label = fmt.Sprintf("%s (%s)", s.Path, s.FD)
		}
		fmt.Println(display.Dim("  " + label))
	}
	fmt.Println()

	var paths []string
	for _, s := range sources {
		paths = append(paths, s.Path)
	}

	tailPath, err := exec.LookPath("tail")
	if err != nil {
		return fmt.Errorf("tail not found in PATH")
	}

	args := []string{"tail"}
	if logsFollow {
		args = append(args, "-f")
	}
	args = append(args, paths...)

	return syscall.Exec(tailPath, args, os.Environ())
}

// execLogStream uses macOS `log stream` to show process logs.
func execLogStream(pid int) error {
	logPath, err := exec.LookPath("log")
	if err != nil {
		return fmt.Errorf("log command not found")
	}

	args := []string{"log", "stream", "--process", strconv.Itoa(pid), "--style", "compact"}
	return syscall.Exec(logPath, args, os.Environ())
}

// tailProcFD tries to tail /proc/<pid>/fd/1 (stdout) on Linux.
func tailProcFD(pid int) error {
	stdoutPath := fmt.Sprintf("/proc/%d/fd/1", pid)
	stderrPath := fmt.Sprintf("/proc/%d/fd/2", pid)

	var paths []string
	for _, p := range []string{stdoutPath, stderrPath} {
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}

	if len(paths) == 0 {
		return fmt.Errorf("no log sources found for PID %d\n\n%s",
			pid,
			display.Dim("The process may be writing to a terminal or pipe that cannot be captured.\n"+
				"Try restarting the process with output redirected to a file:\n"+
				"  command > output.log 2>&1"))
	}

	// Print what we're tailing
	for _, p := range paths {
		name := "stdout"
		if strings.HasSuffix(p, "/2") {
			name = "stderr"
		}
		fmt.Println(display.Dim(fmt.Sprintf("  %s (%s)", p, name)))
	}
	fmt.Println()

	tailPath, err := exec.LookPath("tail")
	if err != nil {
		return fmt.Errorf("tail not found in PATH")
	}

	args := []string{"tail"}
	if logsFollow {
		args = append(args, "-f")
	}
	args = append(args, paths...)

	return syscall.Exec(tailPath, args, os.Environ())
}
