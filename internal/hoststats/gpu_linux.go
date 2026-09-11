package hoststats

import "os/exec"

// probeGPU is Linux's GPU source: nvidia-smi and amdgpu sysfs.
func probeGPU() gpuReader { return probeLinuxGPUs(exec.LookPath, "/sys") }
