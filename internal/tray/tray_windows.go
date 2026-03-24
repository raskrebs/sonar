//go:build windows

package tray

import "fmt"

func Run(detach bool) error {
	return fmt.Errorf("system tray is currently only supported on macOS")
}
