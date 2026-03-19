package ports

import (
	"fmt"
	"os/exec"
	"strings"
)

// ScanRemote discovers all TCP ports in LISTEN state on a remote host via SSH.
// It tries ss first, then falls back to lsof.
func ScanRemote(host string) ([]ListeningPort, error) {
	cmd := exec.Command("ssh", host, "ss -tlnp 2>/dev/null || lsof -iTCP -sTCP:LISTEN -n -P 2>/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return nil, fmt.Errorf("ssh to %s failed: %w", host, err)
		}
		// Some output was produced; the error may be from a non-zero exit code
		// on the remote side. Try to parse what we got.
	}

	output := strings.TrimSpace(string(out))
	if output == "" {
		return nil, nil
	}

	// Detect whether the output is from ss or lsof by inspecting the header.
	// ss output starts with "State" or "Netid"; lsof starts with "COMMAND".
	firstLine := output
	if idx := strings.IndexByte(output, '\n'); idx >= 0 {
		firstLine = output[:idx]
	}

	if strings.HasPrefix(firstLine, "COMMAND") {
		return parseLsof(output), nil
	}
	// Default to ss parsing (header starts with State, Netid, or similar)
	return parseSS(output), nil
}
