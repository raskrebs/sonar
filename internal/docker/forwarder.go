package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
//
// Lima and Colima forward through processes that any other Lima VM, or any
// ssh, could also be running, so those count only when they belong to the
// instance behind the current Docker context, which lima resolves.
func heldByForwarder(p *ports.ListeningPort, lima func() (limaInstance, bool)) bool {
	name := p.Process
	if name == "" {
		name = firstWord(p.Command)
	}
	if name == "" {
		return true
	}
	if isForwarderProcess(name) {
		return true
	}
	switch {
	case isSSH(name):
		inst, ok := lima()
		return ok && inst.holdsSSHMaster(p.Command)
	case isLimactl(name):
		inst, ok := lima()
		return ok && inst.holdsHostAgent(p.Command)
	}
	return false
}

// limaInstance is a Lima VM: its name and its instance directory, which holds
// lima.yaml, the host agent's ha.sock and the ssh ControlMaster's ssh.sock.
type limaInstance struct {
	name string
	dir  string
}

// Lima's default port forwarder, and so Colima's, is ssh: the instance's
// ControlMaster holds every published port, and ssh retitles that process
// "ssh: <instance dir>/ssh.sock [mux]". Verified on macOS with Colima 0.10.3:
// a container published with `-p 47311:80` is held by a process lsof reports
// as "ssh" and ps as "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]".
// "ssh" is far too common a name to trust on its own (an `ssh -L` tunnel is
// exactly the kind of native process #95 is about), and any ssh can name its
// ControlPath ssh.sock, so only the master of this instance's directory counts.
func (l limaInstance) holdsSSHMaster(command string) bool {
	return strings.TrimSpace(command) == "ssh: "+filepath.Join(l.dir, "ssh.sock")+" [mux]"
}

// Lima's gRPC port forwarder is the host agent itself. Verified on macOS with
// Colima 0.10.3: the agent runs as `limactl hostagent --pidfile <dir>/ha.pid
// --socket <dir>/ha.sock ... <instance>`, which lsof reports as "limactl", and
// with `--port-forwarder grpc` it holds the published ports. Another Lima VM
// has its own agent, so the instance name and the agent's paths must match.
func (l limaInstance) holdsHostAgent(command string) bool {
	args := strings.Fields(command)
	if len(args) < 3 || !isLimactl(args[0]) || args[1] != "hostagent" || args[len(args)-1] != l.name {
		return false
	}
	for i := 2; i+1 < len(args)-1; i++ {
		if (args[i] == "--pidfile" || args[i] == "--socket") && filepath.Dir(args[i+1]) != l.dir {
			return false
		}
	}
	return true
}

// limaInstanceFor maps a Docker endpoint to the Lima instance serving it, from
// the socket's path alone:
//
//   - Lima: <LIMA_HOME>/<instance>/sock/docker.sock
//   - Colima: <COLIMA_HOME>/<profile>/docker.sock, whose instance is
//     <COLIMA_HOME>/_lima/colima for the default profile and
//     <COLIMA_HOME>/_lima/colima-<profile> otherwise
//
// The directory must also look like a Lima instance, so an endpoint that only
// happens to end in docker.sock (Docker's own /var/run/docker.sock) maps to
// nothing.
func limaInstanceFor(host string) (limaInstance, bool) {
	sock, ok := strings.CutPrefix(host, "unix://")
	if !ok || filepath.Base(sock) != "docker.sock" {
		return limaInstance{}, false
	}
	dir := filepath.Dir(filepath.Clean(sock))
	var inst limaInstance
	if filepath.Base(dir) == "sock" {
		inst.dir = filepath.Dir(dir)
		inst.name = filepath.Base(inst.dir)
	} else {
		inst.name = "colima"
		if profile := filepath.Base(dir); profile != "default" {
			inst.name += "-" + profile
		}
		inst.dir = filepath.Join(filepath.Dir(dir), "_lima", inst.name)
	}
	if !isLimaInstanceDir(inst.dir) {
		return limaInstance{}, false
	}
	return inst, true
}

// isLimaInstanceDir reports whether dir holds a Lima instance's config or its
// running host agent's socket.
func isLimaInstanceDir(dir string) bool {
	for _, f := range []string{"lima.yaml", "ha.sock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// currentDockerHost returns the endpoint of the current Docker context, the
// one the container list came from. `docker context inspect` reads the CLI's
// configuration only, so it answers while the daemon is wedged. It is a
// variable so tests can answer for it.
var currentDockerHost = func() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	cmd, stop := cli("context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	defer stop()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// currentLimaInstance is the Lima instance behind the current Docker context,
// if there is one.
func currentLimaInstance() (limaInstance, bool) {
	return limaInstanceFor(currentDockerHost())
}

// isSSH reports whether a scanner's process name is ssh. Read from the
// retitled command line instead, the first word is "ssh:".
func isSSH(name string) bool {
	name = normalizeProcess(name)
	return name == "ssh" || name == "ssh:"
}

// isLimactl reports whether a scanner's process name is Lima's limactl.
func isLimactl(name string) bool {
	return normalizeProcess(name) == "limactl"
}

// fillForwarderCommands fills in, with one ps call, the command line of every
// ssh or limactl process that holds a port the containers publish, so
// heldByForwarder can tell this instance's forwarder from any other. Container
// data is stamped before ports.Enrich, so a scan's rows have no command line
// yet.
func fillForwarderCommands(pp []ports.ListeningPort, published map[int]*container) {
	var pids []string
	for i := range pp {
		if _, ok := published[pp[i].Port]; ok && pp[i].Command == "" && pp[i].PID > 0 &&
			(isSSH(pp[i].Process) || isLimactl(pp[i].Process)) {
			pids = append(pids, strconv.Itoa(pp[i].PID))
		}
	}
	if len(pids) == 0 {
		return
	}
	commands := processCommands(pids)
	for i := range pp {
		if c, ok := commands[pp[i].PID]; ok && pp[i].Command == "" {
			pp[i].Command = c
		}
	}
}

// processCommands returns the command line of each pid, keyed by pid. It is a
// variable so tests can answer for ps.
var processCommands = func(pids []string) map[int]string {
	out, err := exec.Command("ps", "-o", "pid=,command=", "-p", strings.Join(pids, ",")).Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	commands := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		pid, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		commands[n] = strings.TrimSpace(command)
	}
	return commands
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
