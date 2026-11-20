package docker

import (
	"os"
	"path/filepath"
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
		{"limactl", false},               // Lima's host agent needs its command line too
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

// limaFixture makes a Lima instance directory named name under a fresh home,
// with the file a running instance has.
func limaFixture(t *testing.T, name string) limaInstance {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "_lima", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lima.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return limaInstance{name: name, dir: dir}
}

func TestHeldByForwarder(t *testing.T) {
	inst := limaInstance{name: "colima", dir: "/Users/me/.colima/_lima/colima"}
	current := func() (limaInstance, bool) { return inst, true }
	none := func() (limaInstance, bool) { return limaInstance{}, false }
	agent := "/opt/homebrew/bin/limactl hostagent --pidfile /Users/me/.colima/_lima/colima/ha.pid " +
		"--socket /Users/me/.colima/_lima/colima/ha.sock --guestagent /opt/homebrew/share/lima/lima-guestagent.Linux-aarch64.gz colima"
	cases := []struct {
		label string
		row   ports.ListeningPort
		lima  func() (limaInstance, bool)
		want  bool
	}{
		{"forwarder by name", ports.ListeningPort{Process: "com.docke"}, nil, true},
		{"native by name", ports.ListeningPort{Process: "node"}, nil, false},
		{"the name wins over the command", ports.ListeningPort{Process: "node", Command: "/usr/bin/docker-proxy -proto tcp"}, nil, false},
		{"forwarder by command", ports.ListeningPort{Command: "/Applications/Docker.app/Contents/MacOS/com.docker.backend services"}, nil, true},
		{"forwarder by quoted command", ports.ListeningPort{Command: `"C:\Program Files\Docker\Docker\resources\com.docker.backend.exe" --x`}, nil, true},
		{"native by command", ports.ListeningPort{Command: "node server.js"}, nil, false},
		// Windows' netstat names nothing before ports.Enrich: no evidence, so
		// the cached list keeps applying.
		{"owner unknown", ports.ListeningPort{PID: 4242}, nil, true},
		// Lima and Colima forward through the instance's ssh ControlMaster.
		{"lima ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]"}, current, true},
		{"lima ssh master by command", ports.ListeningPort{Command: "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]"}, current, true},
		{"another instance's ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.colima/_lima/colima-dev/ssh.sock [mux]"}, current, false},
		{"plain Lima VM's ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.lima/default/ssh.sock [mux]"}, current, false},
		{"ssh master named ssh.sock", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.ssh/work/ssh.sock [mux]"}, current, false},
		{"lima ssh master, Docker not on Lima", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.colima/_lima/colima/ssh.sock [mux]"}, none, false},
		{"ssh tunnel", ports.ListeningPort{Process: "ssh", Command: "ssh -N -L 3000:localhost:3000 dev"}, current, false},
		{"other ssh master", ports.ListeningPort{Process: "ssh", Command: "ssh: /Users/me/.ssh/cm-me@dev:22 [mux]"}, current, false},
		{"ssh without a command line", ports.ListeningPort{Process: "ssh"}, current, false},
		// Lima's gRPC forwarder is the instance's host agent.
		{"lima host agent", ports.ListeningPort{Process: "limactl", Command: agent}, current, true},
		{"another instance's host agent", ports.ListeningPort{Process: "limactl", Command: strings.ReplaceAll(agent, "colima", "colima-dev")}, current, false},
		{"same name, other LIMA_HOME", ports.ListeningPort{Process: "limactl", Command: strings.ReplaceAll(agent, "/Users/me/.colima/_lima/", "/Users/me/.lima/")}, current, false},
		{"limactl, not the host agent", ports.ListeningPort{Process: "limactl", Command: "limactl shell colima"}, current, false},
		{"host agent, Docker not on Lima", ports.ListeningPort{Process: "limactl", Command: agent}, none, false},
		{"limactl without a command line", ports.ListeningPort{Process: "limactl"}, current, false},
	}
	for _, c := range cases {
		lima := c.lima
		if lima == nil {
			lima = none
		}
		if got := heldByForwarder(&c.row, lima); got != c.want {
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
// container's port through ssh or the host agent, which a scan reports with no
// command line. Only the forwarder of the instance behind the current Docker
// context counts.
func TestEnrichKeepsLimaForwardedPorts(t *testing.T) {
	inst := limaFixture(t, "colima")
	other := limaFixture(t, "colima-dev")
	home := filepath.Dir(filepath.Dir(inst.dir))
	if err := os.MkdirAll(filepath.Join(home, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	origHost := currentDockerHost
	currentDockerHost = func() string { return "unix://" + filepath.Join(home, "default", "docker.sock") }
	t.Cleanup(func() { currentDockerHost = origHost })

	commands := map[int]string{
		77: "ssh: " + filepath.Join(inst.dir, "ssh.sock") + " [mux]",
		78: "ssh -N -L 3001:localhost:3001 dev",
		80: "limactl hostagent --pidfile " + filepath.Join(inst.dir, "ha.pid") + " --socket " + filepath.Join(inst.dir, "ha.sock") + " colima",
		81: "ssh: " + filepath.Join(other.dir, "ssh.sock") + " [mux]",
	}
	var asked [][]string
	orig := processCommands
	processCommands = func(pids []string) map[int]string {
		asked = append(asked, pids)
		return commands
	}
	t.Cleanup(func() { processCommands = orig })

	containers := parsePS([]byte("web\tnginx:itest\t0.0.0.0:3000->80/tcp, 0.0.0.0:3001->81/tcp, 0.0.0.0:3003->82/tcp\t\t\t\n"))
	pp := []ports.ListeningPort{
		{Port: 3000, PID: 77, Process: "ssh"},
		{Port: 3001, PID: 78, Process: "ssh"},
		{Port: 3002, PID: 79, Process: "ssh"}, // no container publishes it
		{Port: 3000, PID: 80, Process: "limactl"},
		{Port: 3003, PID: 81, Process: "ssh"}, // another Colima profile's VM
	}
	enrichFrom(pp, containers)

	if len(asked) != 1 || strings.Join(asked[0], ",") != "77,78,80,81" {
		t.Errorf("ps was asked for %v, want one call for 77,78,80,81", asked)
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
	if p := pp[4]; p.Type == ports.PortTypeDocker || p.DockerContainer != "" {
		t.Errorf("a port another Lima VM holds was stamped from the container list: %+v", p)
	}
}

func TestLimaInstanceFor(t *testing.T) {
	colima := limaFixture(t, "colima")
	colimaHome := filepath.Dir(filepath.Dir(colima.dir))
	dev := limaInstance{name: "colima-dev", dir: filepath.Join(colimaHome, "_lima", "colima-dev")}
	if err := os.MkdirAll(dev.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev.dir, "ha.sock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	plain := limaFixture(t, "docker")

	cases := []struct {
		label string
		host  string
		want  limaInstance
		ok    bool
	}{
		{"colima default profile", "unix://" + filepath.Join(colimaHome, "default", "docker.sock"), colima, true},
		{"colima named profile", "unix://" + filepath.Join(colimaHome, "dev", "docker.sock"), dev, true},
		{"lima instance", "unix://" + filepath.Join(plain.dir, "sock", "docker.sock"), plain, true},
		{"colima profile without a VM", "unix://" + filepath.Join(colimaHome, "other", "docker.sock"), limaInstance{}, false},
		{"docker's own socket", "unix:///var/run/docker.sock", limaInstance{}, false},
		{"docker desktop", "unix:///Users/me/.docker/run/docker.sock", limaInstance{}, false},
		{"tcp endpoint", "tcp://127.0.0.1:2375", limaInstance{}, false},
		{"no context", "", limaInstance{}, false},
	}
	for _, c := range cases {
		got, ok := limaInstanceFor(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: limaInstanceFor(%q) = %+v, %v; want %+v, %v", c.label, c.host, got, ok, c.want, c.ok)
		}
	}
}
