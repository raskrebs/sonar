package ports

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// parseDarwinStats parses: %cpu rss state lstart...
// lstart format: "Mon Jan  2 15:04:05 2006" (5 fields)
func parseDarwinStats(p *ListeningPort, fields []string) {
	if len(fields) < 8 {
		return
	}

	if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
		p.CPUPercent = v
	}
	if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
		p.MemoryRSS = v * 1024 // ps reports RSS in KB
	}
	p.State = decodeState(fields[2])

	// lstart is the remaining fields (e.g. "Wed Mar 18 10:30:00 2026")
	lstart := strings.Join(fields[3:], " ")
	p.StartTime = lstart
	p.Uptime = computeUptime(lstart)

	// macOS: get thread count via proc_info or ps -M
	p.ThreadCount = countThreadsDarwin(p.PID)
}

// parseLinuxStats parses: %cpu rss nlwp state lstart...
func parseLinuxStats(p *ListeningPort, fields []string) {
	if len(fields) < 9 {
		return
	}

	if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
		p.CPUPercent = v
	}
	if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
		p.MemoryRSS = v * 1024
	}
	if v, err := strconv.Atoi(fields[2]); err == nil {
		p.ThreadCount = v
	}
	p.State = decodeState(fields[3])

	lstart := strings.Join(fields[4:], " ")
	p.StartTime = lstart
	p.Uptime = computeUptime(lstart)
}

// countThreadsDarwin gets thread count on macOS via ps -M.
func countThreadsDarwin(pid int) int {
	out, err := exec.Command("ps", "-M", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// First line is the header, remaining lines are threads
	if len(lines) <= 1 {
		return 1
	}
	return len(lines) - 1
}

// countConnections counts established TCP connections to a specific port.
func countConnections(port int) int {
	portStr := strconv.Itoa(port)

	var out []byte
	var err error

	if runtime.GOOS == "darwin" {
		out, err = exec.Command("lsof", "-iTCP:"+portStr, "-sTCP:ESTABLISHED", "-n", "-P").Output()
	} else {
		out, err = exec.Command("ss", "-tn", "state", "established", fmt.Sprintf("sport = :%s", portStr)).Output()
	}
	if err != nil {
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return 0
	}
	// Subtract header line; divide by 2 for lsof (each connection shows local + remote)
	count := len(lines) - 1
	if runtime.GOOS == "darwin" {
		count = count / 2
	}
	return count
}

// decodeState converts the single-char process state to a human-readable string.
func decodeState(s string) string {
	if s == "" {
		return ""
	}
	switch s[0] {
	case 'R':
		return "running"
	case 'S':
		return "sleeping"
	case 'D':
		return "disk sleep"
	case 'T':
		return "stopped"
	case 'Z':
		return "zombie"
	case 'I':
		return "idle"
	case 'U':
		return "uninterruptible"
	default:
		return strings.ToLower(s)
	}
}

// computeUptime parses an lstart string and returns a human-readable duration.
func computeUptime(lstart string) string {
	// lstart format varies but common: "Wed Mar 18 10:30:00 2026"
	layouts := []string{
		"Mon Jan  2 15:04:05 2006",
		"Mon Jan 2 15:04:05 2006",
	}

	var t time.Time
	var err error
	for _, layout := range layouts {
		t, err = time.Parse(layout, lstart)
		if err == nil {
			break
		}
	}
	if err != nil {
		return ""
	}

	d := time.Since(t)
	return formatDuration(d)
}

// formatDuration returns a concise human-readable duration string.
func formatDuration(d time.Duration) string {
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

// FormatBytes returns a human-readable byte size.
func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
