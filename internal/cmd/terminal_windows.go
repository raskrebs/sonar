//go:build windows

package cmd

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	enableVirtualTerminalProcessing uint32 = 0x0004
)

var vtEnabled bool

// enableVTProcessing enables ANSI escape code support on Windows consoles.
func enableVTProcessing() {
	if vtEnabled {
		return
	}
	vtEnabled = true

	handle := os.Stdout.Fd()
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return
	}
	procSetConsoleMode.Call(handle, uintptr(mode|enableVirtualTerminalProcessing))
}
