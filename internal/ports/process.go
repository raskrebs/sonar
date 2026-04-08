package ports

import (
	"fmt"
	"strings"
)

// FindByPort scans and returns the port entry matching the given port number.
// If bindIP is non-empty, it filters to the specific bind address.
// If multiple entries match the same port and no bindIP is specified,
// an error is returned listing the available bind addresses.
func FindByPort(port int, bindIP string) (*ListeningPort, error) {
	all, err := Scan()
	if err != nil {
		return nil, err
	}

	matches := FindAllByPort(port, all)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no process found listening on port %d", port)
	}

	// If a specific bind IP was requested, filter to it
	if bindIP != "" {
		for _, p := range matches {
			if p.BindAddress == bindIP {
				return &p, nil
			}
		}
		return nil, fmt.Errorf("no process found listening on %s:%d", bindIP, port)
	}

	// Single match — no ambiguity
	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple matches — ask user to disambiguate
	var addrs []string
	for _, p := range matches {
		addrs = append(addrs, p.BindAddress)
	}
	return nil, fmt.Errorf("port %d is bound to multiple addresses: %s\nUse --ip to specify which one (e.g. --ip %s)",
		port, strings.Join(addrs, ", "), addrs[0])
}

// Kill sends a signal to the process listening on the given port.
func Kill(port int, bindIP string, force bool) error {
	lp, err := FindByPort(port, bindIP)
	if err != nil {
		return err
	}

	return KillPID(lp.PID, force)
}
