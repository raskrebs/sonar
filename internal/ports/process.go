package ports

import (
	"fmt"
)

// FindByPort scans and returns the port entry matching the given port number.
func FindByPort(port int) (*ListeningPort, error) {
	all, err := Scan()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Port == port {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("no process found listening on port %d", port)
}

// Kill sends a signal to the process listening on the given port.
func Kill(port int, force bool) error {
	lp, err := FindByPort(port)
	if err != nil {
		return err
	}

	return KillPID(lp.PID, force)
}
