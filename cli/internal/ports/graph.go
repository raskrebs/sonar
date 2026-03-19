package ports

import (
	"bufio"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Connection represents an established TCP connection between two listening services.
type Connection struct {
	FromPort    int    `json:"from_port"`
	FromProcess string `json:"from_process"`
	ToPort      int    `json:"to_port"`
	ToProcess   string `json:"to_process"`
}

// BuildGraph discovers established TCP connections between listening ports.
// For each listening port's PID, it finds outbound connections whose remote
// port matches another listening port on localhost.
func BuildGraph(listening []ListeningPort) ([]Connection, error) {
	switch runtime.GOOS {
	case "darwin":
		return buildGraphLsof(listening)
	case "linux":
		return buildGraphSS(listening)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// buildGraphLsof uses `lsof -i -n -P` on macOS to find ESTABLISHED connections.
func buildGraphLsof(listening []ListeningPort) ([]Connection, error) {
	// Build lookup maps
	portToProcess := make(map[int]string)
	pidToPort := make(map[int]int)
	for _, lp := range listening {
		portToProcess[lp.Port] = lp.DisplayName()
		pidToPort[lp.PID] = lp.Port
	}

	out, err := exec.Command("lsof", "-i", "-n", "-P").CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("lsof: %w\n%s", err, out)
	}

	type connKey struct {
		fromPort int
		toPort   int
	}
	seen := make(map[connKey]bool)
	var connections []Connection

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}

		// Only care about ESTABLISHED connections
		if fields[9] != "(ESTABLISHED)" {
			continue
		}

		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		// This PID must own one of our listening ports
		fromPort, ok := pidToPort[pid]
		if !ok {
			continue
		}

		// NAME field looks like "127.0.0.1:52345->127.0.0.1:5432"
		name := fields[8]
		arrowIdx := strings.Index(name, "->")
		if arrowIdx < 0 {
			continue
		}

		remote := name[arrowIdx+2:]
		colonIdx := strings.LastIndex(remote, ":")
		if colonIdx < 0 {
			continue
		}

		toPort, err := strconv.Atoi(remote[colonIdx+1:])
		if err != nil {
			continue
		}

		// The remote port must be one of our listening ports
		toProcess, ok := portToProcess[toPort]
		if !ok {
			continue
		}

		// Skip self-connections and deduplicates
		if toPort == fromPort {
			continue
		}
		key := connKey{fromPort, toPort}
		if seen[key] {
			continue
		}
		seen[key] = true

		connections = append(connections, Connection{
			FromPort:    fromPort,
			FromProcess: portToProcess[fromPort],
			ToPort:      toPort,
			ToProcess:   toProcess,
		})
	}

	return connections, nil
}

// buildGraphSS uses `ss -tnp` on Linux to find ESTABLISHED connections.
func buildGraphSS(listening []ListeningPort) ([]Connection, error) {
	portToProcess := make(map[int]string)
	pidToPort := make(map[int]int)
	for _, lp := range listening {
		portToProcess[lp.Port] = lp.DisplayName()
		pidToPort[lp.PID] = lp.Port
	}

	out, err := exec.Command("ss", "-tnp").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ss: %w\n%s", err, out)
	}

	type connKey struct {
		fromPort int
		toPort   int
	}
	seen := make(map[connKey]bool)
	var connections []Connection

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Scan() // skip header
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		// State must be ESTAB
		if fields[0] != "ESTAB" {
			continue
		}

		// Extract PID from the process column (e.g. users:(("node",pid=1234,fd=5)))
		pid := 0
		for _, f := range fields {
			if strings.HasPrefix(f, "users:") {
				if pidIdx := strings.Index(f, "pid="); pidIdx >= 0 {
					pidStr := f[pidIdx+4:]
					if commaIdx := strings.IndexByte(pidStr, ','); commaIdx >= 0 {
						pidStr = pidStr[:commaIdx]
					}
					pid, _ = strconv.Atoi(pidStr)
				}
			}
		}

		if pid == 0 {
			continue
		}

		fromPort, ok := pidToPort[pid]
		if !ok {
			continue
		}

		// Remote address is field 4 (e.g. 127.0.0.1:5432)
		remote := fields[4]
		colonIdx := strings.LastIndex(remote, ":")
		if colonIdx < 0 {
			continue
		}

		toPort, err := strconv.Atoi(remote[colonIdx+1:])
		if err != nil {
			continue
		}

		toProcess, ok := portToProcess[toPort]
		if !ok {
			continue
		}

		if toPort == fromPort {
			continue
		}
		key := connKey{fromPort, toPort}
		if seen[key] {
			continue
		}
		seen[key] = true

		connections = append(connections, Connection{
			FromPort:    fromPort,
			FromProcess: portToProcess[fromPort],
			ToPort:      toPort,
			ToProcess:   toProcess,
		})
	}

	return connections, nil
}
