package display

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/raskrebs/sonar/internal/ports"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Available column names.
var DefaultColumns = []string{"port", "process", "container", "image", "containerport", "url"}

// AllColumns lists every supported column name.
var AllColumns = []string{
	"port", "process", "pid", "type", "url",
	"cpu", "mem", "threads", "uptime", "state", "connections",
	"container", "image", "containerport", "compose", "project",
	"user", "bind", "ip",
	"health", "latency",
}

// TableOptions controls table rendering.
type TableOptions struct {
	Filter  string   // "docker", "user", "system", or "" for all
	SortBy  string   // "port", "pid", "name", "type"
	Columns []string // which columns to show (empty = default)
}

// RenderTable writes a colored, formatted table of listening ports.
func RenderTable(w io.Writer, pp []ports.ListeningPort, opts TableOptions) {
	filtered := filterPorts(pp, opts.Filter)
	sortPorts(filtered, opts.SortBy)

	if len(filtered) == 0 {
		fmt.Fprintln(w, "No listening ports found.")
		return
	}

	cols := opts.Columns
	if len(cols) == 0 {
		cols = DefaultColumns
	}

	// Build header row
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = Bold(strings.ToUpper(columnLabel(c)))
	}

	// Build data rows
	rows := make([][]string, len(filtered))
	for r, p := range filtered {
		rows[r] = make([]string, len(cols))
		for c, col := range cols {
			rows[r][c] = columnValue(p, col)
		}
	}

	// Calculate column widths from visible text (strip ANSI)
	widths := make([]int, len(cols))
	for i, h := range headers {
		if vw := visibleLen(h); vw > widths[i] {
			widths[i] = vw
		}
	}
	for _, row := range rows {
		for i, cell := range row {
			if vw := visibleLen(cell); vw > widths[i] {
				widths[i] = vw
			}
		}
	}

	const gap = 3

	// Print header
	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(w, strings.Repeat(" ", gap))
		}
		fmt.Fprint(w, padRight(h, widths[i]))
	}
	fmt.Fprintln(w)

	// Print rows
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(w, strings.Repeat(" ", gap))
			}
			fmt.Fprint(w, padRight(cell, widths[i]))
		}
		fmt.Fprintln(w)
	}

	// Summary line
	dockerCount, userCount, systemCount := countTypes(filtered)
	parts := []string{}
	if dockerCount > 0 {
		parts = append(parts, fmt.Sprintf("%d docker", dockerCount))
	}
	if userCount > 0 {
		parts = append(parts, fmt.Sprintf("%d user", userCount))
	}
	if systemCount > 0 {
		parts = append(parts, fmt.Sprintf("%d system", systemCount))
	}
	summary := fmt.Sprintf("%d ports", len(filtered))
	if len(parts) > 0 {
		summary = fmt.Sprintf("%d ports (%s)", len(filtered), strings.Join(parts, ", "))
	}
	fmt.Fprintf(w, "\n%s\n", Dim(summary))
}

// visibleLen returns the visible length of a string, ignoring ANSI escape codes.
func visibleLen(s string) int {
	return len(ansiRegex.ReplaceAllString(s, ""))
}

// padRight pads a string to the given visible width with spaces.
func padRight(s string, width int) string {
	vl := visibleLen(s)
	if vl >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vl)
}

func columnLabel(col string) string {
	switch col {
	case "containerport":
		return "CPORT"
	case "compose":
		return "SERVICE"
	case "project":
		return "PROJECT"
	case "bind":
		return "BIND"
	case "ip":
		return "IPVER"
	case "cpu":
		return "CPU%"
	case "mem":
		return "MEM"
	case "threads":
		return "THR"
	case "connections":
		return "CONN"
	case "health":
		return "HEALTH"
	case "latency":
		return "LATENCY"
	default:
		return col
	}
}

func columnValue(p ports.ListeningPort, col string) string {
	switch col {
	case "port":
		return BoldCyan(fmt.Sprintf("%d", p.Port))
	case "process":
		return colorProcess(p)
	case "pid":
		return fmt.Sprintf("%d", p.PID)
	case "type":
		return colorType(p.Type)
	case "url":
		return Underline(p.URL())
	case "container":
		return p.DockerContainer
	case "image":
		return p.DockerImage
	case "containerport":
		if p.DockerContainerPort > 0 {
			return fmt.Sprintf("%d", p.DockerContainerPort)
		}
		return ""
	case "compose":
		return p.DockerComposeService
	case "project":
		return p.DockerComposeProject
	case "user":
		return p.User
	case "bind":
		return p.BindAddress
	case "ip":
		return p.IPVersion
	case "cpu":
		return fmt.Sprintf("%.1f", p.CPUPercent)
	case "mem":
		if p.MemoryRSS > 0 {
			return ports.FormatBytes(p.MemoryRSS)
		}
		return ""
	case "threads":
		if p.ThreadCount > 0 {
			return fmt.Sprintf("%d", p.ThreadCount)
		}
		return ""
	case "uptime":
		return p.Uptime
	case "state":
		return colorState(p.State)
	case "connections":
		return fmt.Sprintf("%d", p.Connections)
	case "health":
		return colorHealth(p.HealthStatus)
	case "latency":
		if p.HealthLatency > 0 {
			return fmt.Sprintf("%dms", p.HealthLatency.Milliseconds())
		}
		return ""
	default:
		return ""
	}
}

func colorProcess(p ports.ListeningPort) string {
	name := p.DisplayName()
	// If it's Docker and we have the image, show "name (image)"
	if p.Type == ports.PortTypeDocker && p.DockerImage != "" && p.DockerContainer != "" {
		display := p.DisplayName()
		if display != p.DockerImage {
			name = fmt.Sprintf("%s (%s)", display, p.DockerImage)
		}
	}
	switch p.Type {
	case ports.PortTypeDocker:
		return Magenta(name)
	case ports.PortTypeSystem:
		return Yellow(name)
	default:
		return Green(name)
	}
}

func colorState(state string) string {
	switch state {
	case "running":
		return Green(state)
	case "sleeping", "idle":
		return Dim(state)
	case "zombie":
		return Red(state)
	case "stopped":
		return Yellow(state)
	default:
		return state
	}
}

func colorHealth(status string) string {
	switch status {
	case "healthy":
		return Green(status)
	case "unhealthy", "refused", "timeout":
		return Red(status)
	case "non-http":
		return Yellow(status)
	default:
		return status
	}
}

func colorType(t ports.PortType) string {
	switch t {
	case ports.PortTypeDocker:
		return Magenta(t.String())
	case ports.PortTypeSystem:
		return Yellow(t.String())
	default:
		return Green(t.String())
	}
}

// FilterPorts returns only ports matching the given type filter.
func FilterPorts(pp []ports.ListeningPort, filter string) []ports.ListeningPort {
	return filterPorts(pp, filter)
}

func filterPorts(pp []ports.ListeningPort, filter string) []ports.ListeningPort {
	if filter == "" {
		return pp
	}

	var result []ports.ListeningPort
	for _, p := range pp {
		if p.Type.String() == filter {
			result = append(result, p)
		}
	}
	return result
}

func sortPorts(pp []ports.ListeningPort, sortBy string) {
	switch sortBy {
	case "pid":
		sort.Slice(pp, func(i, j int) bool { return pp[i].PID < pp[j].PID })
	case "name":
		sort.Slice(pp, func(i, j int) bool {
			return strings.ToLower(pp[i].DisplayName()) < strings.ToLower(pp[j].DisplayName())
		})
	case "type":
		sort.Slice(pp, func(i, j int) bool { return pp[i].Type < pp[j].Type })
	default: // "port" or empty
		sort.Slice(pp, func(i, j int) bool { return pp[i].Port < pp[j].Port })
	}
}

func countTypes(pp []ports.ListeningPort) (docker, user, system int) {
	for _, p := range pp {
		switch p.Type {
		case ports.PortTypeDocker:
			docker++
		case ports.PortTypeUser:
			user++
		case ports.PortTypeSystem:
			system++
		}
	}
	return
}
