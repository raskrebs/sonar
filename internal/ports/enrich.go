package ports

import (
	"encoding/csv"
	"fmt"
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
			pp[i].IsApp = isDesktopApp(pp[i].Command, pp[i].Process, pp[i].PID)
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

	// Connection counts — on Windows, fetch netstat once and reuse the output
	if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-ano").Output()
		if err == nil {
			output := string(out)
			for i := range pp {
				pp[i].Connections = countConnectionsNetstat(output, strconv.Itoa(pp[i].Port))
			}
		}
	} else {
		for i := range pp {
			pp[i].Connections = countConnections(pp[i].Port)
		}
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

	if runtime.GOOS == "windows" {
		return batchGetCommandsWindows(pidStrs)
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

// batchGetCommandsWindows fetches command lines via PowerShell Get-CimInstance on Windows.
func batchGetCommandsWindows(pidStrs []string) map[int]string {
	result := make(map[int]string)

	// Build WMI filter: "ProcessId=123 or ProcessId=456"
	var conditions []string
	for _, p := range pidStrs {
		conditions = append(conditions, "ProcessId="+p)
	}
	filter := strings.Join(conditions, " or ")

	psCmd := fmt.Sprintf(
		"Get-CimInstance Win32_Process -Filter '%s' | Select-Object ProcessId,CommandLine | ConvertTo-Csv -NoTypeInformation",
		filter,
	)

	out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
	if err != nil {
		return result
	}

	r := csv.NewReader(strings.NewReader(strings.TrimSpace(string(out))))
	records, err := r.ReadAll()
	if err != nil {
		return result
	}

	// CSV columns: "ProcessId","CommandLine"
	for i, record := range records {
		if i == 0 {
			continue // skip header
		}
		if len(record) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(record[0]))
		if err != nil {
			continue
		}
		cmd := strings.TrimSpace(record[1])
		if cmd != "" {
			result[pid] = cmd
		}
	}
	return result
}

// batchEnrichProcessStats fetches CPU, memory, state, uptime for all non-Docker
// ports in a single ps call (or PowerShell on Windows).
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

	// Build PID -> port lookup
	pidMap := make(map[int]*ListeningPort)
	for _, p := range nativePorts {
		pidMap[p.PID] = p
	}

	if runtime.GOOS == "windows" {
		batchEnrichProcessStatsWindows(pidStrs, pidMap)
		return
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

// batchEnrichProcessStatsWindows uses PowerShell Get-Process to fetch stats.
func batchEnrichProcessStatsWindows(pidStrs []string, pidMap map[int]*ListeningPort) {
	psCmd := fmt.Sprintf(
		"Get-Process -Id %s -ErrorAction SilentlyContinue | Select-Object Id,CPU,WorkingSet64,@{N='ThreadCount';E={$_.Threads.Count}},@{N='StartTime';E={$_.StartTime.ToString('o')}} | ConvertTo-Csv -NoTypeInformation",
		strings.Join(pidStrs, ","),
	)

	out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).Output()
	if err != nil {
		return
	}

	r := csv.NewReader(strings.NewReader(strings.TrimSpace(string(out))))
	records, err := r.ReadAll()
	if err != nil {
		return
	}

	// CSV columns: "Id","CPU","WorkingSet64","ThreadCount","StartTime"
	for i, record := range records {
		if i == 0 {
			continue // skip header
		}
		if len(record) < 5 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(record[0]))
		if err != nil {
			continue
		}
		p, ok := pidMap[pid]
		if !ok {
			continue
		}
		parseWindowsStats(p, record[1:])
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

// isDesktopApp detects if the command belongs to a desktop application or
// OS-level system service that is not relevant to development.
func isDesktopApp(command string, process string, pid int) bool {
	if runtime.GOOS == "windows" {
		return isWindowsDesktopApp(command, process, pid)
	}

	// macOS / Linux
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

// isWindowsDesktopApp detects Windows desktop apps and system services.
// PID 0 (System Idle) and PID 4 (System) own ports like 135, 139, 445.
func isWindowsDesktopApp(command string, process string, pid int) bool {
	if pid == 0 || pid == 4 {
		return true
	}

	lower := strings.ToLower(command)
	if lower == "" {
		lower = strings.ToLower(process)
	}
	if lower == "" {
		// No command or process name — system service not visible without elevation
		return true
	}

	// Windows system services
	if strings.Contains(lower, `\windows\`) {
		return true
	}
	// User-installed desktop apps (AppData\Local houses Discord, Cursor, Slack, etc.)
	if strings.Contains(lower, `\appdata\`) {
		return true
	}
	// Microsoft Store apps
	if strings.Contains(lower, `\windowsapps\`) {
		return true
	}
	// Known desktop app executable names (for cases where only the process name is available)
	knownApps := []string{
		"discord", "cursor", "slack", "spotify", "figma", "zoom",
		"teams", "onedrive", "dropbox", "githubdesktop", "notion",
		"telegram", "whatsapp", "1password", "bitwarden",
		"chrome", "firefox", "msedge", "brave", "opera",
		"explorer", "searchhost", "widgets",
	}
	// Extract the base executable name without .exe
	baseName := strings.ToLower(process)
	if i := strings.LastIndex(baseName, `\`); i >= 0 {
		baseName = baseName[i+1:]
	}
	baseName = strings.TrimSuffix(baseName, ".exe")
	// Also strip any trailing quote artifacts from netstat parsing
	baseName = strings.TrimRight(baseName, `"`)

	for _, app := range knownApps {
		if strings.Contains(baseName, app) {
			return true
		}
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
