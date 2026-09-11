package state

// LocalhostName is the reserved name of the machine the daemon runs on. It is
// always present in Snapshot.Hosts, and every row a client sees without a
// registered remote host carries it.
const LocalhostName = "localhost"

// Host status values. localhost is always Connected; the rest describe an SSH
// bridge to a registered remote host (milestone 3, step 3A.2 onwards).
const (
	HostConnecting   = "connecting"
	HostConnected    = "connected"
	HostUnreachable  = "unreachable"
	HostOutdated     = "outdated"
	HostIncompatible = "incompatible"
)

// AllHostStatuses is the enum used by schema generation.
var AllHostStatuses = []string{
	HostConnecting, HostConnected, HostUnreachable, HostOutdated, HostIncompatible,
}

// Host is one machine sonar knows about: its identity, the state of the
// connection to it, and its load. The daemon publishes exactly one Host for
// itself, named "localhost"; step 3A.2 adds the registered remote hosts.
//
// Every load field is a pointer because a platform, or a moment, may not be
// able to answer:
//
//   - CPUPercent is null on the first tick everywhere. It is a delta between
//     two samples, and one sample is no measurement.
//   - Load is null on Windows, which has no load average. On Linux and macOS
//     it is exactly three entries: 1, 5 and 15 minutes.
//   - UptimeS, Memory* and Disk* are null only when the underlying read fails
//     (a container with no /proc, a permission error); all three platforms
//     provide them.
//   - StatusReason is null while nothing is wrong. For localhost it carries
//     the collector's error when a tick could not read the machine's load.
//   - GPUs is null when the GPUs were not collected: a platform with no
//     source (Windows, a Linux box with neither nvidia-smi nor an amdgpu
//     card), the daemon's first seconds, a read that failed or timed out, or
//     a remote daemon too old to send the field. An empty list means they
//     were collected and there are none.
//
// Kernel is the release string ("6.8.0-40-generic", "25.6.0", "10.0.26100")
// and is empty, not null, when the platform lookup fails.
type Host struct {
	Name            string    `json:"name"`
	Address         string    `json:"address"`
	Status          string    `json:"status" jsonschema:"enum=connecting,enum=connected,enum=unreachable,enum=outdated,enum=incompatible"`
	StatusReason    *string   `json:"status_reason" jsonschema:"nullable"`
	DaemonVersion   string    `json:"daemon_version"`
	ProtocolVersion string    `json:"protocol_version"`
	LatencyMs       int64     `json:"latency_ms"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch"`
	Kernel          string    `json:"kernel"`
	UptimeS         *int64    `json:"uptime_s" jsonschema:"nullable"`
	CPUPercent      *float64  `json:"cpu_percent" jsonschema:"nullable"`
	Load            []float64 `json:"load" jsonschema:"nullable"`
	MemoryUsed      *int64    `json:"memory_used_bytes" jsonschema:"nullable"`
	MemoryTotal     *int64    `json:"memory_total_bytes" jsonschema:"nullable"`
	DiskUsed        *int64    `json:"disk_used_bytes" jsonschema:"nullable"`
	DiskTotal       *int64    `json:"disk_total_bytes" jsonschema:"nullable"`
	DiskPath        string    `json:"disk_path"`
	GPUs            []GPU     `json:"gpus" jsonschema:"nullable"`
	Ports           int       `json:"ports"`
	Groups          int       `json:"groups"`
	LastSeen        string    `json:"last_seen"`
}

// Key is the delta identity: the host name.
func (h Host) Key() string { return h.Name }

// PrefixKey is the delta-key rule for a row that may have come from a remote
// host: local rows (host empty or "localhost") keep the key they have always
// had, and only a remote row is namespaced with "<host>/". Keeping the local
// form byte-identical is what lets a client built against the pre-3A.2
// protocol keep working without a major bump (remote-hosts spec, decision 1);
// the row's own `host` field is the disambiguator for everyone else.
func PrefixKey(host, key string) string {
	if IsLocalhost(host) {
		return key
	}
	return host + "/" + key
}

// IsLocalhost reports whether a row's `host` names the daemon's own machine.
// The empty string counts: a row built by a client that predates the field, or
// by the CLI's direct scan, is a local row.
func IsLocalhost(host string) bool {
	return host == "" || host == LocalhostName
}

// HostOf is IsLocalhost's other half: the name a row should be filtered under,
// with the empty string normalised to "localhost".
func HostOf(host string) string {
	if host == "" {
		return LocalhostName
	}
	return host
}
