package ports

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type PortType int

const (
	PortTypeSystem PortType = iota
	PortTypeUser
	PortTypeDocker
)

func (pt PortType) String() string {
	switch pt {
	case PortTypeSystem:
		return "system"
	case PortTypeUser:
		return "user"
	case PortTypeDocker:
		return "docker"
	default:
		return "unknown"
	}
}

type ListeningPort struct {
	Port        int
	PID         int
	Process     string // short name (e.g. "node")
	Command     string // full cmdline from ps
	User        string
	BindAddress string
	IPVersion   string // "IPv4" / "IPv6"
	Type        PortType
	IsApp       bool // true for desktop apps (Figma, Discord, etc.)

	// Process stats
	CPUPercent  float64 // CPU usage percentage
	MemoryRSS   int64   // resident set size in bytes
	ThreadCount int     // number of threads
	StartTime   string  // process start time (raw from ps)
	Uptime      string  // human-readable uptime
	State       string  // process state (running, sleeping, etc.)
	Connections int     // number of established connections on this port

	// Health check fields
	HealthStatus  string
	HealthCode    int
	HealthLatency time.Duration

	// Docker fields (empty if not Docker)
	DockerContainer      string
	DockerImage          string
	DockerComposeService string
	DockerComposeProject string
	DockerContainerPort  int
}

// URL returns the localhost URL for this port.
func (lp *ListeningPort) URL() string {
	return fmt.Sprintf("http://localhost:%d", lp.Port)
}

// DisplayName returns the best human-readable name for the process.
// Priority: compose service > container name > short command > process name.
func (lp *ListeningPort) DisplayName() string {
	if lp.DockerComposeService != "" {
		return lp.DockerComposeService
	}
	if lp.DockerContainer != "" {
		return lp.DockerContainer
	}
	if lp.Command != "" {
		return shortCommand(lp.Command)
	}
	return lp.Process
}

// shortCommand returns a short display form of a full command line.
// e.g. "/usr/local/bin/node server.js --port=3000" -> "node server.js"
func shortCommand(cmd string) string {
	// For .app bundles, extract the app name directly from the full string
	// before splitting (paths may contain spaces like "Application Support")
	if idx := strings.Index(cmd, ".app/"); idx >= 0 {
		appPath := cmd[:idx]
		return filepath.Base(appPath)
	}

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd
	}
	base := filepath.Base(parts[0])
	if len(parts) == 1 {
		return base
	}
	// Include the first non-flag argument if it exists
	for _, arg := range parts[1:] {
		if !strings.HasPrefix(arg, "-") {
			return base + " " + filepath.Base(arg)
		}
	}
	return base
}
