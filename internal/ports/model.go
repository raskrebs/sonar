package ports

import (
	"fmt"
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
	ParentCmd   string // parent process cmdline (for unwrapping reload supervisors)
	Cwd         string // process working directory
	ServiceUnit string // systemd unit name (Linux) or launchd label (macOS)
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

// PortKey returns a unique identifier for this listening socket (port + bind address).
func (lp *ListeningPort) PortKey() string {
	return fmt.Sprintf("%d:%s", lp.Port, lp.BindAddress)
}

// URL returns the HTTP URL for this port using its bind address.
// For wildcard binds (0.0.0.0), localhost is used.
func (lp *ListeningPort) URL() string {
	host := lp.BindAddress
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d", host, lp.Port)
}

// FindAllByPort returns all listening entries matching the given port number.
func FindAllByPort(port int, all []ListeningPort) []ListeningPort {
	var matches []ListeningPort
	for _, p := range all {
		if p.Port == port {
			matches = append(matches, p)
		}
	}
	return matches
}

// DisplayName returns the best human-readable name for the process.
// Priority: compose service > container name > service manager unit >
// resolved cmdline (with parent + cwd context) > process name.
//
// All signal collection (cmdline, parent cmdline, cwd, service unit) is done
// during Enrich. This method is a pure view over those fields and is safe to
// call from anywhere without I/O.
func (lp *ListeningPort) DisplayName() string {
	if lp.DockerComposeService != "" {
		return lp.DockerComposeService
	}
	if lp.DockerContainer != "" {
		return lp.DockerContainer
	}
	// Desktop apps: their cmdline already yields a clean .app bundle name
	// and their cwd / launchd label point to internal app state, not a
	// project. Skip the supervisor / cwd / service-unit signals for them.
	if lp.IsApp {
		if lp.Command != "" {
			if name := interpreterAwareBasename(lp.Command); name != "" {
				return name
			}
		}
		return lp.Process
	}
	if lp.ServiceUnit != "" {
		return lp.ServiceUnit
	}
	if name := resolveProcessName(lp.Command, lp.ParentCmd, lp.Cwd); name != "" {
		return name
	}
	return lp.Process
}
