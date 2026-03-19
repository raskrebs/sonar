package display

import (
	"encoding/json"
	"io"

	"github.com/raskrebs/sonar/internal/ports"
)

// JSONPort is the JSON-serializable representation of a listening port.
type JSONPort struct {
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	Process     string `json:"process"`
	Command     string `json:"command,omitempty"`
	User        string `json:"user,omitempty"`
	BindAddress string `json:"bind_address,omitempty"`
	IPVersion   string `json:"ip_version,omitempty"`
	Type        string `json:"type"`
	URL         string `json:"url"`

	// Process stats
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryRSS   int64   `json:"memory_rss_bytes"`
	ThreadCount int     `json:"thread_count,omitempty"`
	Uptime      string  `json:"uptime,omitempty"`
	State       string  `json:"state,omitempty"`
	Connections int     `json:"connections"`

	// Health check fields
	HealthStatus     string `json:"health_status,omitempty"`
	HealthCode       int    `json:"health_code,omitempty"`
	HealthLatencyMs  int64  `json:"health_latency_ms,omitempty"`

	// Docker fields
	DockerContainer      string `json:"docker_container,omitempty"`
	DockerImage          string `json:"docker_image,omitempty"`
	DockerComposeService string `json:"docker_compose_service,omitempty"`
	DockerComposeProject string `json:"docker_compose_project,omitempty"`
	DockerContainerPort  int    `json:"docker_container_port,omitempty"`
}

// RenderJSON writes the ports as a JSON array.
func RenderJSON(w io.Writer, pp []ports.ListeningPort) error {
	out := make([]JSONPort, len(pp))
	for i, p := range pp {
		out[i] = JSONPort{
			Port:                 p.Port,
			PID:                  p.PID,
			Process:              p.Process,
			Command:              p.Command,
			User:                 p.User,
			BindAddress:          p.BindAddress,
			IPVersion:            p.IPVersion,
			Type:                 p.Type.String(),
			URL:                  p.URL(),
			CPUPercent:           p.CPUPercent,
			MemoryRSS:            p.MemoryRSS,
			ThreadCount:          p.ThreadCount,
			Uptime:               p.Uptime,
			State:                p.State,
			Connections:          p.Connections,
			HealthStatus:         p.HealthStatus,
			HealthCode:           p.HealthCode,
			HealthLatencyMs:      p.HealthLatency.Milliseconds(),
			DockerContainer:      p.DockerContainer,
			DockerImage:          p.DockerImage,
			DockerComposeService: p.DockerComposeService,
			DockerComposeProject: p.DockerComposeProject,
			DockerContainerPort:  p.DockerContainerPort,
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
