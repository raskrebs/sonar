package docker

import (
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/ports"
)

func TestIsForwarderProcess(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// What the scanners really report for Docker's forwarders.
		{"com.docke", true},              // macOS lsof, truncated to 9 (verified)
		{"com.docker.backend", true},     // lsof +c 0, ps comm basename
		{"com.docker.back", true},        // Linux comm, truncated to 15
		{"com.docker.backend.exe", true}, // Windows tasklist
		{"COM.DOCKER.BACKEND.EXE", true}, // Windows is case-insensitive
		{"vpnkit", true},                 // older Docker Desktop
		{"vpnkit.exe", true},             // older Docker Desktop on Windows
		{"docker-proxy", true},           // Linux userland proxy
		{"docker-pr", true},              // docker-proxy through lsof
		{"rootlesskit", true},            // rootless Docker
		{"rootlessport", true},           // rootless Docker
		{"slirp4netns", true},            // rootless Docker
		{"limactl", true},                // Lima's gRPC forwarder (Colima --port-forwarder grpc)
		{"/usr/bin/docker-proxy", true},  // a path, not a bare name
		{`C:\Docker\vpnkit.exe`, true},   // a Windows path
		{"node", false},                  // the native processes #95 is about
		{"python3", false},               // ...
		{"postgres", false},              // ...
		{"docker", false},                // the CLI never holds a published port
		{"ssh", false},                   // Lima's ssh forwarder needs its command line too
		{"dockerd", false},               // nor does the engine on Linux
		{"com.docker.backendx", false},   // longer than a real name is not a truncation
		{"vpn", false},                   // a short prefix is not a truncation
		{"docker-", false},               // under minTruncatedName
		{"", false},
	}
	for _, c := range cases {
		if got := isForwarderProcess(c.name); got != c.want {
			t.Errorf("isForwarderProcess(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHeldByForwarder(t *testing.T) {
	cases := []struct {
		label string
		row   ports.ListeningPort
		want  bool
	}{
		{"forwarder by name", ports.ListeningPort{Process: "com.docke"}, true},
		{"native by name", ports.ListeningPort{Process: "node"}, false},
		{"the name wins over the command", ports.ListeningPort{Process: "node", Command: "/usr/bin/docker-proxy -proto tcp"}, false},
		{"forwarder by command", ports.ListeningPort{Command: "/Applications/Docker.app/Contents/MacOS/com.docker.backend services"}, true},
		{"forwarder by quoted command", ports.ListeningPort{Command: `"C:\Program Files\Docker\Docker\resources\com.docker.backend.exe" --x`}, true},
		{"native by command", ports.ListeningPort{Command: "node server.js"}, false},
		// Windows' netstat names nothing before ports.Enrich: no evidence, so
		// the cached list keeps applying.
		{"owner unknown", ports.ListeningPort{PID: 4242}, true},
		// Lima and Colima forward through the instance's ssh ControlMaster.
		{"lima ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]"}, true},
		{"lima ssh master by command", ports.ListeningPort{Command: "ssh: /home/me/.lima/default/ssh.sock [mux]"}, true},
		{"ssh tunnel", ports.ListeningPort{Process: "ssh", Command: "ssh -N -L 3000:localhost:3000 dev"}, false},
		{"other ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.ssh/cm-me@dev:22 [mux]"}, false},
		{"ssh without a command line", ports.ListeningPort{Process: "ssh"}, false},
	}
	for _, c := range cases {
		if got := heldByForwarder(&c.row); got != c.want {
			t.Errorf("%s: heldByForwarder = %v, want %v", c.label, got, c.want)
		}
	}
}

// TestEnrichSkipsAPortANativeProcessHolds is issue #95 at the enrichment step:
// the cached list says itest-web-1 publishes 3000, but node holds 3000 now.
func TestEnrichSkipsAPortANativeProcessHolds(t *testing.T) {
	containers := parsePS([]byte("itest-web-1\tnginx:itest\t0.0.0.0:3000->80/tcp\tweb\titestproj\t/src/app\n"))

	native := []ports.ListeningPort{{Port: 3000, PID: 501, Process: "node", Type: ports.PortTypeUser}}
	enrichFrom(native, containers)
	if p := native[0]; p.Type == ports.PortTypeDocker || p.DockerContainer != "" || p.DockerImage != "" ||
		p.DockerComposeProject != "" || p.DockerComposeService != "" || p.DockerContainerPort != 0 {
		t.Errorf("a port node holds was stamped from the cached list: %+v", p)
	}

	forwarded := []ports.ListeningPort{{Port: 3000, PID: 77, Process: "com.docke"}}
	enrichFrom(forwarded, containers)
	p := forwarded[0]
	if p.Type != ports.PortTypeDocker || p.DockerContainer != "itest-web-1" || p.DockerImage != "nginx:itest" ||
		p.DockerComposeService != "web" || p.DockerComposeProject != "itestproj" ||
		p.DockerComposeWorkingDir != "/src/app" || p.DockerContainerPort != 80 {
		t.Errorf("the forwarder's port lost its container data: %+v", p)
	}
}

// TestEnrichKeepsLimaForwardedPorts is issue #98: Colima and Lima publish a
// container's port through ssh, which a scan reports with no command line.
func TestEnrichKeepsLimaForwardedPorts(t *testing.T) {
	commands := map[int]string{
		77: "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]",
		78: "ssh -N -L 3001:localhost:3001 dev",
	}
	var asked [][]string
	orig := processCommands
	processCommands = func(pids []string) map[int]string {
		asked = append(asked, pids)
		return commands
	}
	t.Cleanup(func() { processCommands = orig })

	containers := parsePS([]byte("web\tnginx:itest\t0.0.0.0:3000->80/tcp, 0.0.0.0:3001->81/tcp\t\t\t\n"))
	pp := []ports.ListeningPort{
		{Port: 3000, PID: 77, Process: "ssh"},
		{Port: 3001, PID: 78, Process: "ssh"},
		{Port: 3002, PID: 79, Process: "ssh"}, // no container publishes it
		{Port: 3000, PID: 80, Process: "limactl"},
	}
	enrichFrom(pp, containers)

	if len(asked) != 1 || strings.Join(asked[0], ",") != "77,78" {
		t.Errorf("ps was asked for %v, want one call for 77,78", asked)
	}
	if p := pp[0]; p.Type != ports.PortTypeDocker || p.DockerContainer != "web" || p.DockerContainerPort != 80 {
		t.Errorf("the port Lima's ssh master holds lost its container data: %+v", p)
	}
	if p := pp[1]; p.Type == ports.PortTypeDocker || p.DockerContainer != "" {
		t.Errorf("a port an ssh tunnel holds was stamped from the container list: %+v", p)
	}
	if p := pp[3]; p.Type != ports.PortTypeDocker || p.DockerContainer != "web" {
		t.Errorf("the port Lima's host agent holds lost its container data: %+v", p)
	}
}
