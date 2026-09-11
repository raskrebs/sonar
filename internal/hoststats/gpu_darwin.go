package hoststats

import "os/exec"

// probeGPU is macOS's GPU source: ioreg's IOAccelerator entries.
func probeGPU() gpuReader { return probeDarwinGPUs(exec.LookPath, "/usr/sbin/ioreg") }
