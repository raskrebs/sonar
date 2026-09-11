//go:build !linux && !darwin

package hoststats

// probeGPU has no source on Windows or any other platform yet, so GPUs are
// never collected there and stay null.
func probeGPU() gpuReader { return nil }
