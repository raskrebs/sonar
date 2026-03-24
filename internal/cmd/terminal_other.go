//go:build !windows

package cmd

// enableVTProcessing is a no-op on non-Windows platforms.
func enableVTProcessing() {}
