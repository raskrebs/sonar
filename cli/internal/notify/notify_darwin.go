//go:build darwin

package notify

import (
	"fmt"
	"os/exec"
)

func send(title, message string) error {
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	return exec.Command("osascript", "-e", script).Run()
}
