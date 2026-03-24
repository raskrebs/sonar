package ports

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// LogSource represents a source of log output for a process.
type LogSource struct {
	Path string
	FD   string // "stdout", "stderr", or empty for log files
}

// FindLogSources discovers log files and output streams for a process via lsof.
// On Windows, log discovery is not supported and returns nil.
func FindLogSources(pid int) []LogSource {
	if runtime.GOOS == "windows" {
		return nil
	}

	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-Fn", "-a").Output()
	if err != nil {
		return nil
	}

	var sources []LogSource
	seen := make(map[string]bool)

	lines := strings.Split(string(out), "\n")
	var currentFD string
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 'f':
			currentFD = line[1:]
		case 'n':
			path := line[1:]
			if currentFD == "1" || currentFD == "2" {
				if isReadableFile(path) && !seen[path] {
					seen[path] = true
					fdName := "stdout"
					if currentFD == "2" {
						fdName = "stderr"
					}
					sources = append(sources, LogSource{Path: path, FD: fdName})
				}
			} else if isLogFile(path) && !seen[path] {
				seen[path] = true
				sources = append(sources, LogSource{Path: path})
			}
		}
	}

	return sources
}

// SupportsLogStream returns true if the platform supports log stream (macOS).
func SupportsLogStream() bool {
	return runtime.GOOS == "darwin"
}

// isReadableFile returns true if the path looks like a regular file (not a pipe/socket/device).
func isReadableFile(path string) bool {
	return strings.HasPrefix(path, "/") &&
		!strings.HasPrefix(path, "/dev/") &&
		!strings.Contains(path, "->")
}

// isLogFile returns true if the path looks like a log file.
func isLogFile(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	if strings.HasPrefix(path, "/dev/") {
		return false
	}
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".log") ||
		strings.HasSuffix(lower, ".out") ||
		strings.Contains(lower, "/log/") ||
		strings.Contains(lower, "/logs/")
}
