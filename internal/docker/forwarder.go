package docker

import (
	"strings"

	"github.com/raskrebs/sonar/internal/ports"
)

// Docker does not hand a host port to the container: a helper process on the
// host holds the listening socket and forwards what arrives into the VM or the
// container's network namespace. That helper is the only honest sign, without
// asking Docker anything, that a host port really is a container's — which is
// what step 5A.9 needs, because the check has to hold while Docker is wedged
// and the cached container list cannot be refreshed (issue #95).
//
// The names are what a port scanner reports as the process: lsof's COMMAND on
// macOS, ss's comm on Linux, tasklist's image name on Windows. Both lsof and
// comm truncate (lsof to 9 characters by default, comm to 15), so a name is
// also matched as a prefix of a full name — see forwarderProcess.
var forwarderNames = []string{
	// Docker Desktop's backend holds every published port on macOS and
	// Windows; it is `com.docker.backend.exe` there. Verified on macOS with
	// Docker Engine 29.2.1 (Docker Desktop): a container published with `-p 47311:80`
	// is held by /Applications/Docker.app/Contents/MacOS/com.docker.backend,
	// which lsof reports as the truncated "com.docke".
	"com.docker.backend",
	// Older Docker Desktop builds forwarded through vpnkit, either as its own
	// binary or under the com.docker prefix.
	"vpnkit",
	"com.docker.vpnkit",
	"com.docker.proxy",
	// The Linux engine's userland proxy. With `userland-proxy: false` nothing
	// listens on the host at all — the port is DNAT'd — so such a port never
	// reaches a scan and never needs this check.
	"docker-proxy",
	// Rootless Docker forwards through RootlessKit's port driver instead.
	"rootlesskit",
	"rootlessport",
	"slirp4netns",
}

// minTruncatedName is the shortest prefix accepted as a truncated forwarder
// name. lsof truncates to 9 characters by default, so every name a scanner can
// report is at least that long; requiring 8 keeps a short native process name
// from matching one of the longer forwarders by accident.
const minTruncatedName = 8

// heldByForwarder reports whether the cached container list may speak for this
// port: true when Docker's own forwarder holds it, false when some other
// process does.
//
// A port whose owner the scan could not name at all is not evidence either
// way, so the container list still applies to it. That is the case on Windows,
// where `netstat -ano` reports pids without names and ports.Enrich only fills
// the names in after this pass — the check is inert there, and the behaviour
// is exactly what it was before 5A.9.
func heldByForwarder(p *ports.ListeningPort) bool {
	name := p.Process
	if name == "" {
		name = firstWord(p.Command)
	}
	if name == "" {
		return true
	}
	return isForwarderProcess(name)
}

// isForwarderProcess reports whether a process name is one of Docker's port
// forwarders, allowing for the truncation lsof and comm apply.
func isForwarderProcess(name string) bool {
	name = normalizeProcess(name)
	if name == "" {
		return false
	}
	for _, want := range forwarderNames {
		if name == want {
			return true
		}
		if len(name) >= minTruncatedName && len(name) < len(want) && strings.HasPrefix(want, name) {
			return true
		}
	}
	return false
}

// normalizeProcess reduces a scanner's process name to something comparable:
// lower case, no directory, no Windows executable suffix.
func normalizeProcess(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// firstWord is the executable of a command line, used only as a fallback when
// the scanner reported no process name. A quoted executable (a Windows path
// with spaces in it) is read up to its closing quote.
func firstWord(command string) string {
	command = strings.TrimSpace(command)
	if rest, ok := strings.CutPrefix(command, `"`); ok {
		if i := strings.IndexByte(rest, '"'); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	if i := strings.IndexAny(command, " \t"); i >= 0 {
		command = command[:i]
	}
	return command
}
