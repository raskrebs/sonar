package fakedaemon

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/state"
)

// Fixture is the world a fake daemon serves. Every value is fixed, so golden
// tests over `ports.list` and the MCP tools stay stable across machines.
type Fixture struct {
	DaemonVersion string
	StartedAt     string
	Capabilities  []string
	Ports         []state.Port
	Groups        []state.Group
	Sessions      []state.SessionRecord
	Hosts         []state.Host
}

// FixtureTime is the instant every fixture pretends it is.
const FixtureTime = "2026-09-06T10:00:00Z"

// DefaultFixture is a plausible developer machine: a `sonar start` group with
// an api and a vite dev server (one of them started by an agent session), a
// docker compose project, an ssh daemon, and a desktop app that a read hides
// unless the caller asks for apps.
func DefaultFixture() Fixture {
	return Fixture{
		DaemonVersion: "0.5.1",
		StartedAt:     FixtureTime,
		// The capability list `main` publishes today (contract §22).
		Capabilities: []string{"state", "ports.read", "ports.kill", "store", "streams", "groups", "runs"},
		Ports:        DefaultPorts(),
		Groups:       DefaultGroups(),
		Sessions:     DefaultSessions(),
		Hosts:        DefaultHosts(),
	}
}

// DefaultHosts is the fixture's `hosts` collection: the one row every daemon
// publishes for the machine it runs on, with the load of a developer laptop
// halfway through a working day.
func DefaultHosts() []state.Host {
	return []state.Host{{
		Name: state.LocalhostName, Address: state.LocalhostName,
		Status: state.HostConnected, DaemonVersion: "0.5.1", ProtocolVersion: "1.0.0",
		LatencyMs: 0, OS: "linux", Arch: "amd64", Kernel: "6.8.0-40-generic",
		UptimeS: i64p(93_600), CPUPercent: f64p(12.4),
		Load:       []float64{1.24, 0.98, 0.71},
		MemoryUsed: i64p(9 << 30), MemoryTotal: i64p(32 << 30),
		DiskUsed: i64p(412 << 30), DiskTotal: i64p(931 << 30), DiskPath: "/",
		Ports: 6, Groups: 2, LastSeen: FixtureTime,
	}}
}

// DefaultPorts is the fixture's port table, sorted by port the way a scan
// publishes it.
func DefaultPorts() []state.Port {
	return []state.Port{
		{
			Port: 3000, BindAddress: "127.0.0.1", IPVersion: "IPv4",
			URL: "http://localhost:3000", PID: 4101, PPID: 4100,
			Process: "node", DisplayName: "api", Name: strp("api"),
			Command: "node server.js", Cwd: "/home/dev/shop",
			ProjectRoot: strp("/home/dev/shop"),
			Group:       strp("shop"), GroupSource: srcp(state.SourceStart),
			Session: &state.Session{
				ID: "claude-code:9f2c", Tool: "claude-code", Label: "shop",
				Worktree: "", Branch: "main", Detected: false,
			},
			Type: state.TypeUser, User: "dev",
			Run:         &state.Run{ID: "run-7f3a", Group: "shop", Name: "api", RootPID: 4100},
			Stats:       &state.Stats{CPUPercent: 1.5, MemoryRSS: 84 << 20, ThreadCount: 11, Uptime: "12m0s", State: "running", Connections: 2},
			Health:      &state.Health{Status: state.HealthOK, Code: 200, LatencyMs: 3, Reason: "healthy"},
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
		{
			Port: 5173, BindAddress: "127.0.0.1", IPVersion: "IPv4",
			URL: "http://localhost:5173", PID: 4210, PPID: 4209,
			Process: "node", DisplayName: "vite", Name: nil,
			Command: "vite dev", Cwd: "/home/dev/shop/web",
			ProjectRoot: strp("/home/dev/shop"),
			Group:       strp("shop"), GroupSource: srcp(state.SourceAuto),
			Type: state.TypeUser, User: "dev",
			Stats:       &state.Stats{CPUPercent: 0.4, MemoryRSS: 61 << 20, ThreadCount: 9, Uptime: "9m0s", State: "running"},
			Health:      &state.Health{Status: state.HealthOK, Code: 200, LatencyMs: 2, Reason: "healthy"},
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
		{
			Port: 5432, BindAddress: "0.0.0.0", IPVersion: "IPv4",
			URL: "http://localhost:5432", PID: 3300, PPID: 1,
			Process: "docker-proxy", DisplayName: "shop-db",
			Command: "postgres", Cwd: "",
			Group: strp("shop-infra"), GroupSource: srcp(state.SourceFile),
			Type: state.TypeDocker, User: "root",
			Health: &state.Health{Status: state.HealthFail, Code: 0, LatencyMs: 0, Reason: "non-http"},
			Docker: &state.Docker{
				Container: "shop-db-1", Image: "postgres:16",
				ComposeService: "db", ComposeProject: "shop-infra", ContainerPort: 5432,
			},
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
		{
			Port: 8080, BindAddress: "0.0.0.0", IPVersion: "IPv4",
			URL: "http://localhost:8080", PID: 3301, PPID: 1,
			Process: "docker-proxy", DisplayName: "shop-gateway",
			Command: "nginx -g daemon off;", Cwd: "",
			Group: strp("shop-infra"), GroupSource: srcp(state.SourceFile),
			Type: state.TypeDocker, User: "root",
			Health: &state.Health{Status: state.HealthOK, Code: 200, LatencyMs: 5, Reason: "healthy"},
			Docker: &state.Docker{
				Container: "shop-gateway-1", Image: "nginx:1.27",
				ComposeService: "gateway", ComposeProject: "shop-infra", ContainerPort: 80,
			},
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
		{
			Port: 22, BindAddress: "0.0.0.0", IPVersion: "IPv4",
			URL: "http://localhost:22", PID: 640, PPID: 1,
			Process: "sshd", DisplayName: "sshd", Command: "/usr/sbin/sshd -D",
			Type: state.TypeSystem, User: "root", ServiceUnit: strp("ssh.service"),
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
		{
			Port: 7000, BindAddress: "127.0.0.1", IPVersion: "IPv4",
			URL: "http://localhost:7000", PID: 900, PPID: 1,
			Process: "ControlCenter", DisplayName: "ControlCenter",
			Command: "/System/Applications/ControlCenter.app",
			Type:    state.TypeSystem, IsApp: true, User: "dev",
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		},
	}
}

// DefaultGroups is the fixture's group table: one `sonar start` group with a
// `sonar.yaml`, one compose project.
func DefaultGroups() []state.Group {
	return []state.Group{
		{
			Name: "shop", Source: state.SourceStart,
			RootDir: strp("/home/dev/shop"), ConfigPath: strp("/home/dev/shop/sonar.yaml"),
			Status: "running", Members: []int{3000, 5173},
			Services: []state.Service{
				{
					Name: "api", Cmd: "node server.js", Cwd: ".", Port: intp(3000),
					Health: strp("/health"), DependsOn: []string{}, Running: true, PortActual: intp(3000),
				},
				{
					Name: "web", Cmd: "vite dev", Cwd: "./web", Port: intp(5173),
					DependsOn: []string{"api"}, Running: true, PortActual: intp(5173),
				},
			},
		},
		{
			Name: "shop-infra", Source: state.SourceFile,
			RootDir: strp("/home/dev/shop"), ConfigPath: strp("/home/dev/shop/docker-compose.yaml"),
			Status: "running", Members: []int{5432, 8080}, Services: []state.Service{},
		},
	}
}

// DefaultSessions is the sessions collection matching the fixture's ports.
// Sessions are spec 2's own addition (slice 2A.4); the fixture carries them
// from the start so the tools that read them can be written against it.
func DefaultSessions() []state.SessionRecord {
	return []state.SessionRecord{{
		Session: state.Session{
			ID: "claude-code:9f2c", Tool: "claude-code", Label: "shop",
			Branch: "main", Detected: false,
		},
		FirstSeen: FixtureTime, LastSeen: FixtureTime,
		Runs: 1, Ports: 1, Groups: 1, Active: true,
	}}
}

// ManyPorts returns n synthetic user ports starting at 4000. It exists for the
// text-cap tests: the renderer truncates at 200 rows.
func ManyPorts(n int) []state.Port {
	out := make([]state.Port, 0, n)
	for i := range n {
		port := 4000 + i
		out = append(out, state.Port{
			Port: port, BindAddress: "127.0.0.1", IPVersion: "IPv4",
			URL:         fmt.Sprintf("http://localhost:%d", port),
			PID:         10000 + i,
			Process:     "node",
			DisplayName: fmt.Sprintf("svc-%d", i),
			Command:     "node worker.js",
			Type:        state.TypeUser, User: "dev",
			ExposedURLs: []string{}, StartedAt: strp(FixtureTime),
		})
	}
	return out
}

func strp(s string) *string { return &s }

func i64p(v int64) *int64 { return &v }

func f64p(v float64) *float64 { return &v }
func intp(i int) *int         { return &i }

func srcp(s state.GroupSource) *state.GroupSource { return &s }
