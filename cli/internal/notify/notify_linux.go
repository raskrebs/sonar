//go:build linux

package notify

import (
	"os/exec"
)

func send(title, message string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		// notify-send not available; silently skip
		return nil
	}
	return exec.Command(path, title, message).Run()
}
