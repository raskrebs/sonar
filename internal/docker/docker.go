package docker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
)

type container struct {
	name           string
	image          string
	portMappings   []portMapping
	composeService string
	composeProject string
}

type portMapping struct {
	hostPort      int
	containerPort int
}

// EnrichPorts queries Docker for running containers and enriches any ports
// that match Docker-published host ports. Fails silently if Docker is unavailable.
func EnrichPorts(pp []ports.ListeningPort) {
	containers, err := listContainers()
	if err != nil || len(containers) == 0 {
		return
	}

	// Build a map of host port -> container info
	hostPortMap := make(map[int]*container)
	mappingMap := make(map[int]portMapping)
	for i := range containers {
		for _, pm := range containers[i].portMappings {
			hostPortMap[pm.hostPort] = &containers[i]
			mappingMap[pm.hostPort] = pm
		}
	}

	for i := range pp {
		c, ok := hostPortMap[pp[i].Port]
		if !ok {
			continue
		}
		pp[i].Type = ports.PortTypeDocker
		pp[i].DockerContainer = c.name
		pp[i].DockerImage = c.image
		pp[i].DockerComposeService = c.composeService
		pp[i].DockerComposeProject = c.composeProject
		pp[i].DockerContainerPort = mappingMap[pp[i].Port].containerPort
	}
}

// ContainerStats holds per-container resource usage.
type ContainerStats struct {
	CPUPercent float64
	MemoryRSS  int64
	PIDs       int
	State      string
	Uptime     string
}

// dockerSocket is the default Docker Engine API socket path.
const dockerSocket = "/var/run/docker.sock"

// apiStatsResponse matches the Docker Engine /containers/{id}/stats response.
type apiStatsResponse struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PrecpuStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
	} `json:"memory_stats"`
	PidsStats struct {
		Current int `json:"current"`
	} `json:"pids_stats"`
}

// apiInspectState matches the relevant fields from /containers/{id}/json.
type apiInspectState struct {
	State struct {
		Status    string `json:"Status"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
}

// AllContainerStatsAsEntries returns stats for all containers as ports.DockerStatsEntry map.
func AllContainerStatsAsEntries() map[string]*ports.DockerStatsEntry {
	allStats := AllContainerStats()
	if len(allStats) == 0 {
		return nil
	}
	result := make(map[string]*ports.DockerStatsEntry, len(allStats))
	for name, s := range allStats {
		result[name] = &ports.DockerStatsEntry{
			CPUPercent: s.CPUPercent,
			MemoryRSS:  s.MemoryRSS,
			PIDs:       s.PIDs,
			State:      s.State,
			Uptime:     s.Uptime,
		}
	}
	return result
}

// AllContainerStats fetches stats for all running containers using the Docker
// Engine API via Unix socket. Stats are fetched in parallel (~1s for CPU sampling).
// Falls back to `docker stats` CLI if the socket is unavailable.
func AllContainerStats() map[string]*ContainerStats {
	result := make(map[string]*ContainerStats)

	containers, err := listContainers()
	if err != nil || len(containers) == 0 {
		return result
	}

	if runtime.GOOS == "windows" {
		return allContainerStatsCLI()
	}

	if _, err := os.Stat(dockerSocket); err != nil {
		return allContainerStatsCLI()
	}

	type statsResult struct {
		name  string
		stats *ContainerStats
	}

	ch := make(chan statsResult, len(containers))

	for _, c := range containers {
		go func(name string) {
			s := fetchContainerStatsAPI(name)
			ch <- statsResult{name: name, stats: s}
		}(c.name)
	}

	for range containers {
		r := <-ch
		if r.stats != nil {
			result[r.name] = r.stats
		}
	}

	return result
}

// fetchContainerStatsAPI fetches stats for a single container via the Docker socket API.
func fetchContainerStatsAPI(name string) *ContainerStats {
	conn, err := net.Dial("unix", dockerSocket)
	if err != nil {
		return nil
	}

	stats := &ContainerStats{}

	statsReq := fmt.Sprintf("GET /containers/%s/stats?stream=false HTTP/1.0\r\nHost: localhost\r\n\r\n", name)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write([]byte(statsReq))
	if err != nil {
		conn.Close()
		return nil
	}

	statsBody, err := readHTTPBody(conn)
	conn.Close()
	if err != nil {
		return nil
	}

	var apiStats apiStatsResponse
	if err := json.Unmarshal(statsBody, &apiStats); err != nil {
		return nil
	}

	cpuDelta := apiStats.CPUStats.CPUUsage.TotalUsage - apiStats.PrecpuStats.CPUUsage.TotalUsage
	sysDelta := apiStats.CPUStats.SystemCPUUsage - apiStats.PrecpuStats.SystemCPUUsage
	ncpu := apiStats.CPUStats.OnlineCPUs
	if ncpu == 0 {
		ncpu = 1
	}
	if sysDelta > 0 {
		stats.CPUPercent = float64(cpuDelta) / float64(sysDelta) * float64(ncpu) * 100.0
	}

	stats.MemoryRSS = int64(apiStats.MemoryStats.Usage)
	stats.PIDs = apiStats.PidsStats.Current

	// Fetch /json for state and start time
	conn2, err := net.Dial("unix", dockerSocket)
	if err != nil {
		return stats
	}
	defer conn2.Close()

	inspectReq := fmt.Sprintf("GET /containers/%s/json HTTP/1.0\r\nHost: localhost\r\n\r\n", name)
	conn2.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = conn2.Write([]byte(inspectReq))
	if err != nil {
		return stats
	}

	inspectBody, err := readHTTPBody(conn2)
	if err != nil {
		return stats
	}

	var inspect apiInspectState
	if err := json.Unmarshal(inspectBody, &inspect); err != nil {
		return stats
	}
	stats.State = inspect.State.Status
	stats.Uptime = computeContainerUptime(inspect.State.StartedAt)

	return stats
}

// readHTTPBody reads an HTTP/1.0 response from a connection and returns the body.
func readHTTPBody(conn net.Conn) ([]byte, error) {
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return io.ReadAll(reader)
}

// allContainerStatsCLI is the fallback using `docker stats` CLI.
func allContainerStatsCLI() map[string]*ContainerStats {
	result := make(map[string]*ContainerStats)

	out, err := exec.Command("docker", "stats", "--no-stream", "--format",
		"{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.PIDs}}").Output()
	if err != nil {
		return result
	}

	scanner := bufio.NewScanner(strings.NewReader(strings.TrimSpace(string(out))))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 4)
		if len(parts) < 4 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		stats := &ContainerStats{}

		cpuStr, _ := strings.CutSuffix(strings.TrimSpace(parts[1]), "%")
		if v, err := strconv.ParseFloat(cpuStr, 64); err == nil {
			stats.CPUPercent = v
		}

		memParts := strings.SplitN(strings.TrimSpace(parts[2]), " / ", 2)
		if len(memParts) >= 1 {
			stats.MemoryRSS = parseMemString(memParts[0])
		}

		if v, err := strconv.Atoi(strings.TrimSpace(parts[3])); err == nil {
			stats.PIDs = v
		}

		result[name] = stats
	}

	// Fetch state/uptime via inspect
	names := make([]string, 0, len(result))
	for name := range result {
		names = append(names, name)
	}
	if len(names) > 0 {
		args := append([]string{"inspect", "--format", "{{.Name}}\t{{.State.Status}}\t{{.State.StartedAt}}"}, names...)
		inspectOut, err := exec.Command("docker", args...).Output()
		if err == nil {
			inspectScanner := bufio.NewScanner(strings.NewReader(strings.TrimSpace(string(inspectOut))))
			for inspectScanner.Scan() {
				parts := strings.SplitN(inspectScanner.Text(), "\t", 3)
				if len(parts) < 3 {
					continue
				}
				name := strings.TrimPrefix(strings.TrimSpace(parts[0]), "/")
				if stats, ok := result[name]; ok {
					stats.State = parts[1]
					stats.Uptime = computeContainerUptime(parts[2])
				}
			}
		}
	}

	return result
}

// parseMemString parses Docker memory strings like "357.7MiB", "1.5GiB", "512KiB".
func parseMemString(s string) int64 {
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	if after, found := strings.CutSuffix(s, "GiB"); found {
		multiplier = 1 << 30
		s = after
	} else if after, found := strings.CutSuffix(s, "MiB"); found {
		multiplier = 1 << 20
		s = after
	} else if after, found := strings.CutSuffix(s, "KiB"); found {
		multiplier = 1 << 10
		s = after
	} else if after, found := strings.CutSuffix(s, "B"); found {
		s = after
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(v * float64(multiplier))
}

// computeContainerUptime parses a Docker ISO 8601 timestamp and returns a human-readable duration.
func computeContainerUptime(startedAt string) string {
	layouts := []string{
		"2006-01-02T15:04:05.999999999Z",
		"2006-01-02T15:04:05.999999999Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, startedAt); err == nil {
			d := time.Since(t)
			return formatContainerDuration(d)
		}
	}
	return ""
}

func formatContainerDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, h)
}

// StopContainer stops a Docker container by name using `docker stop`.
func StopContainer(name string) error {
	if err := exec.Command("docker", "stop", name).Run(); err != nil {
		return fmt.Errorf("failed to stop container %s: %w", name, err)
	}
	return nil
}

// listContainers runs `docker ps` and parses the output.
func listContainers() ([]container, error) {
	format := "{{.Names}}\t{{.Image}}\t{{.Ports}}\t{{.Label \"com.docker.compose.service\"}}\t{{.Label \"com.docker.compose.project\"}}"
	out, err := exec.Command("docker", "ps", "--format", format).Output()
	if err != nil {
		return nil, err
	}

	var containers []container
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 5 {
			continue
		}

		c := container{
			name:           parts[0],
			image:          parts[1],
			portMappings:   parsePorts(parts[2]),
			composeService: parts[3],
			composeProject: parts[4],
		}
		containers = append(containers, c)
	}

	return containers, nil
}

// parsePorts parses Docker port strings like "0.0.0.0:3000->80/tcp, 0.0.0.0:3001->443/tcp".
func parsePorts(raw string) []portMapping {
	if raw == "" {
		return nil
	}

	var mappings []portMapping
	for _, entry := range strings.Split(raw, ", ") {
		// Format: 0.0.0.0:3000->80/tcp or :::3000->80/tcp
		arrowIdx := strings.Index(entry, "->")
		if arrowIdx < 0 {
			continue
		}

		hostPart := entry[:arrowIdx]
		containerPart := entry[arrowIdx+2:]

		// Extract host port (after last colon)
		colonIdx := strings.LastIndex(hostPart, ":")
		if colonIdx < 0 {
			continue
		}
		hostPort, err := strconv.Atoi(hostPart[colonIdx+1:])
		if err != nil {
			continue
		}

		// Extract container port (before /tcp or /udp)
		slashIdx := strings.Index(containerPart, "/")
		cpStr := containerPart
		if slashIdx >= 0 {
			cpStr = containerPart[:slashIdx]
		}
		containerPort, err := strconv.Atoi(cpStr)
		if err != nil {
			continue
		}

		mappings = append(mappings, portMapping{
			hostPort:      hostPort,
			containerPort: containerPort,
		})
	}

	return mappings
}
