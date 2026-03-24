package ports

import (
	"bufio"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Scan discovers all TCP ports in LISTEN state.
func Scan() ([]ListeningPort, error) {
	switch runtime.GOOS {
	case "darwin":
		return scanLsof()
	case "linux":
		return scanSS()
	case "windows":
		return scanNetstat()
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func scanLsof() ([]ListeningPort, error) {
	out, err := exec.Command("lsof", "-iTCP", "-sTCP:LISTEN", "-n", "-P").CombinedOutput()
	if err != nil && len(out) == 0 {
		// lsof exits non-zero when it can't inspect some processes (e.g. non-root),
		// but still outputs the ports it can see. Only fail if there's no output at all.
		return nil, nil
	}

	return parseLsof(string(out)), nil
}

// parseLsof parses the output of lsof -iTCP -sTCP:LISTEN -n -P into ListeningPort entries.
func parseLsof(output string) []ListeningPort {
	seen := make(map[int]bool)
	var results []ListeningPort

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 9 {
			continue
		}

		process := fields[0]
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		user := fields[2]

		// Determine IP version from the TYPE field (IPv4/IPv6)
		ipVersion := "IPv4"
		if fields[4] == "IPv6" {
			ipVersion = "IPv6"
		}

		// NAME field is like *:3000 or 127.0.0.1:3000
		name := fields[8]
		idx := strings.LastIndex(name, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(name[idx+1:])
		if err != nil {
			continue
		}

		bindAddr := name[:idx]
		if bindAddr == "*" {
			bindAddr = "0.0.0.0"
		}

		if seen[port] {
			continue
		}
		seen[port] = true

		results = append(results, ListeningPort{
			Port:        port,
			PID:         pid,
			Process:     process,
			User:        user,
			BindAddress: bindAddr,
			IPVersion:   ipVersion,
		})
	}

	return results
}

func scanNetstat() ([]ListeningPort, error) {
	out, err := exec.Command("netstat", "-ano").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("netstat: %w\n%s", err, out)
	}

	return parseNetstat(string(out)), nil
}

// parseNetstat parses the output of netstat -ano into ListeningPort entries.
// Expected format:
//
//	Proto  Local Address          Foreign Address        State           PID
//	TCP    0.0.0.0:8000           0.0.0.0:0              LISTENING       12345
func parseNetstat(output string) []ListeningPort {
	seen := make(map[int]bool)
	var results []ListeningPort

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		proto := strings.ToUpper(fields[0])
		if proto != "TCP" && proto != "TCPV6" {
			continue
		}

		state := strings.ToUpper(fields[3])
		if state != "LISTENING" {
			continue
		}

		local := fields[1]
		idx := strings.LastIndex(local, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(local[idx+1:])
		if err != nil {
			continue
		}

		pid, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}

		if seen[port] {
			continue
		}
		seen[port] = true

		bindAddr := local[:idx]
		ipVersion := "IPv4"
		if proto == "TCPV6" || strings.Contains(bindAddr, "[") {
			ipVersion = "IPv6"
		}
		if bindAddr == "0.0.0.0" || bindAddr == "[::]" {
			bindAddr = "0.0.0.0"
		}

		results = append(results, ListeningPort{
			Port:        port,
			PID:         pid,
			BindAddress: bindAddr,
			IPVersion:   ipVersion,
		})
	}

	return results
}

func scanSS() ([]ListeningPort, error) {
	out, err := exec.Command("ss", "-tlnp").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ss: %w\n%s", err, out)
	}

	return parseSS(string(out)), nil
}

// parseSS parses the output of ss -tlnp into ListeningPort entries.
func parseSS(output string) []ListeningPort {
	seen := make(map[int]bool)
	var results []ListeningPort

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Scan() // skip header
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		// Local address is field 3, like *:3000 or 0.0.0.0:3000
		local := fields[3]
		idx := strings.LastIndex(local, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(local[idx+1:])
		if err != nil {
			continue
		}

		if seen[port] {
			continue
		}
		seen[port] = true

		bindAddr := local[:idx]
		if bindAddr == "*" {
			bindAddr = "0.0.0.0"
		}

		ipVersion := "IPv4"
		if strings.Contains(bindAddr, "[") {
			ipVersion = "IPv6"
		}

		pid := 0
		process := ""
		for _, f := range fields {
			if strings.HasPrefix(f, "users:") {
				if pidIdx := strings.Index(f, "pid="); pidIdx >= 0 {
					pidStr := f[pidIdx+4:]
					if commaIdx := strings.IndexByte(pidStr, ','); commaIdx >= 0 {
						pidStr = pidStr[:commaIdx]
					}
					pid, _ = strconv.Atoi(pidStr)
				}
				if nameStart := strings.Index(f, "((\""); nameStart >= 0 {
					nameStr := f[nameStart+3:]
					if nameEnd := strings.IndexByte(nameStr, '"'); nameEnd >= 0 {
						process = nameStr[:nameEnd]
					}
				}
			}
		}

		results = append(results, ListeningPort{
			Port:        port,
			PID:         pid,
			Process:     process,
			BindAddress: bindAddr,
			IPVersion:   ipVersion,
		})
	}

	return results
}
