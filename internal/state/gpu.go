package state

// GPU is one graphics processor on a host. Every reading is a pointer for the
// reason Host's are: null means the source could not say, never zero.
//
//   - UtilizationPercent is whole percent, 0–100.
//   - MemoryUsedBytes is the memory the GPU holds: dedicated VRAM on a
//     discrete card, or the system memory the driver has in use on a unified
//     memory machine (Apple silicon).
//   - MemoryTotalBytes is null on unified memory, where the GPU has no memory
//     of its own and any total would be invented; the host's memory_total_bytes
//     is the ceiling there.
type GPU struct {
	Name               string   `json:"name"`
	UtilizationPercent *float64 `json:"utilization_percent" jsonschema:"nullable"`
	MemoryUsedBytes    *int64   `json:"memory_used_bytes" jsonschema:"nullable"`
	MemoryTotalBytes   *int64   `json:"memory_total_bytes" jsonschema:"nullable"`
}
