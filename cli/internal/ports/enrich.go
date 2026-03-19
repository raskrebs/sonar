package ports

import (
	"os/exec"
	"strconv"
	"strings"
)

// Enrich populates the Command field, classifies the port type, and gathers
// process stats (CPU, memory, threads, uptime, state, connections) for each port.
func Enrich(ports []ListeningPort) {
	for i := range ports {
		if ports[i].PID > 0 {
			ports[i].Command = getFullCommand(ports[i].PID)
			enrichStats(&ports[i])
		}
		if ports[i].Type != PortTypeDocker {
			ports[i].Type = ClassifyPort(ports[i].Port)
		}
		if ports[i].Type != PortTypeDocker {
			ports[i].IsApp = isDesktopApp(ports[i].Command)
		}
	}
}

// isDesktopApp detects if the command belongs to a desktop application.
// These are typically .app bundles, system services, or well-known GUI apps.
func isDesktopApp(command string) bool {
	if command == "" {
		return false
	}
	// macOS .app bundles
	if strings.Contains(command, ".app/") {
		return true
	}
	// macOS system services
	if strings.HasPrefix(command, "/System/Library/") || strings.HasPrefix(command, "/usr/libexec/") {
		return true
	}
	return false
}

// getFullCommand retrieves the full command line for a PID using ps.
func getFullCommand(pid int) string {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ClassifyPort returns PortTypeSystem for well-known ports (<1024), else PortTypeUser.
// Docker classification is handled separately by the docker package.
func ClassifyPort(port int) PortType {
	if port < 1024 {
		return PortTypeSystem
	}
	return PortTypeUser
}
