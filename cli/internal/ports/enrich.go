package ports

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Enrich populates the Command field, classifies the port type, and detects
// desktop apps. This is fast and always runs.
func Enrich(pp []ListeningPort) {
	// Batch all PIDs into a single ps call for commands
	commands := batchGetCommands(pp)

	for i := range pp {
		if cmd, ok := commands[pp[i].PID]; ok {
			pp[i].Command = cmd
		}
		if pp[i].Type != PortTypeDocker {
			pp[i].Type = ClassifyPort(pp[i].Port)
			pp[i].IsApp = isDesktopApp(pp[i].Command)
		}
	}
}

// EnrichStats populates CPU, memory, threads, uptime, state, and connections.
// For Docker containers it uses pre-fetched dockerStats.
// For native processes it batches all PIDs into a single ps call.
// Called only when --stats is requested.
func EnrichStats(pp []ListeningPort, dockerStats map[string]*DockerStatsEntry) {
	// Apply Docker stats
	if dockerStats != nil {
		for i := range pp {
			if pp[i].Type == PortTypeDocker && pp[i].DockerContainer != "" {
				if stats, ok := dockerStats[pp[i].DockerContainer]; ok {
					pp[i].CPUPercent = stats.CPUPercent
					pp[i].MemoryRSS = stats.MemoryRSS
					pp[i].ThreadCount = stats.PIDs
					pp[i].State = stats.State
					pp[i].Uptime = stats.Uptime
				}
			}
		}
	}

	// Batch native process stats into a single ps call
	batchEnrichProcessStats(pp)

	// Connection counts
	for i := range pp {
		pp[i].Connections = countConnections(pp[i].Port)
	}
}

// DockerStatsEntry holds pre-fetched per-container stats.
type DockerStatsEntry struct {
	CPUPercent float64
	MemoryRSS  int64
	PIDs       int
	State      string
	Uptime     string
}

// batchGetCommands fetches full command lines for all PIDs in a single ps call.
func batchGetCommands(pp []ListeningPort) map[int]string {
	result := make(map[int]string)
	pids := collectPIDs(pp)
	if len(pids) == 0 {
		return result
	}

	pidStrs := make([]string, len(pids))
	for i, p := range pids {
		pidStrs[i] = strconv.Itoa(p)
	}

	out, err := exec.Command("ps", "-o", "pid=,command=", "-p", strings.Join(pidStrs, ",")).Output()
	if err != nil {
		return result
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		result[pid] = strings.TrimSpace(fields[1])
	}
	return result
}

// batchEnrichProcessStats fetches CPU, memory, state, uptime for all non-Docker
// ports in a single ps call.
func batchEnrichProcessStats(pp []ListeningPort) {
	var nativePorts []*ListeningPort
	for i := range pp {
		if pp[i].Type != PortTypeDocker && pp[i].PID > 0 {
			nativePorts = append(nativePorts, &pp[i])
		}
	}
	if len(nativePorts) == 0 {
		return
	}

	pidStrs := make([]string, len(nativePorts))
	for i, p := range nativePorts {
		pidStrs[i] = strconv.Itoa(p.PID)
	}

	var out []byte
	var err error
	if runtime.GOOS == "darwin" {
		out, err = exec.Command("ps", "-o", "pid=,%cpu=,rss=,state=,lstart=", "-p", strings.Join(pidStrs, ",")).Output()
	} else {
		out, err = exec.Command("ps", "-o", "pid=,%cpu=,rss=,nlwp=,state=,lstart=", "-p", strings.Join(pidStrs, ",")).Output()
	}
	if err != nil {
		return
	}

	// Build PID -> port lookup
	pidMap := make(map[int]*ListeningPort)
	for _, p := range nativePorts {
		pidMap[p.PID] = p
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		p, ok := pidMap[pid]
		if !ok {
			continue
		}
		// Parse remaining fields (skip pid)
		rest := fields[1:]
		if runtime.GOOS == "darwin" {
			parseDarwinStats(p, rest)
			p.ThreadCount = countThreadsDarwin(p.PID)
		} else {
			parseLinuxStats(p, rest)
		}
	}
}

// collectPIDs returns unique non-zero PIDs from the port list.
func collectPIDs(pp []ListeningPort) []int {
	seen := make(map[int]bool)
	var pids []int
	for _, p := range pp {
		if p.PID > 0 && !seen[p.PID] {
			seen[p.PID] = true
			pids = append(pids, p.PID)
		}
	}
	return pids
}

// isDesktopApp detects if the command belongs to a desktop application.
func isDesktopApp(command string) bool {
	if command == "" {
		return false
	}
	if strings.Contains(command, ".app/") {
		return true
	}
	if strings.HasPrefix(command, "/System/Library/") || strings.HasPrefix(command, "/usr/libexec/") {
		return true
	}
	return false
}

// ClassifyPort returns PortTypeSystem for well-known ports (<1024), else PortTypeUser.
func ClassifyPort(port int) PortType {
	if port < 1024 {
		return PortTypeSystem
	}
	return PortTypeUser
}
