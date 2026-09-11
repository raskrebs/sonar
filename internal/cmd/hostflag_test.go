package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// `--host` at the CLI level: a real daemon over a real socket, with a stand-in
// for the SSH bridge behind it. The point is the routing decision and what the
// user sees, not the transport — internal/remote's integration tests drive the
// real bridge.

// fakeHostRouter is one registered host whose daemon is a map of canned
// answers. It records what was forwarded, which is how these tests check that
// `--host` reached the wire rather than being silently dropped.
type fakeHostRouter struct {
	name    string
	replies map[string]any
	calls   []forwarded
}

type forwarded struct {
	host   string
	method string
	params string
}

func (f *fakeHostRouter) Known(name string) bool { return name == f.name }

func (f *fakeHostRouter) Forward(_ context.Context, host, method string, params json.RawMessage) (json.RawMessage, daemon.RemoteStream, error) {
	f.calls = append(f.calls, forwarded{host: host, method: method, params: string(params)})
	reply, ok := f.replies[method]
	if !ok {
		return nil, nil, rpc.NewError(rpc.CodeNotFound, "the fake host does not serve "+method, "")
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return nil, nil, err
	}
	return raw, nil, nil
}

func (f *fakeHostRouter) sawMethod(method string) *forwarded {
	for i := range f.calls {
		if f.calls[i].method == method {
			return &f.calls[i]
		}
	}
	return nil
}

// startDaemonForHostTests runs a daemon whose remote rows and router are the
// test's own, and points the CLI at it.
func startDaemonForHostTests(t *testing.T, router daemon.Router, remote func() state.Rows) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the unix-socket harness does not apply to named pipes")
	}
	dir, err := os.MkdirTemp("", "sn")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")

	loop := scanner.New(scanner.Options{DaemonVersion: "test"})
	srv := daemon.New(daemon.Options{Socket: socket, Version: "test", Scanner: loop})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-srv.Done()
	})
	if err := client.WaitForSocket(ctx, socket, 5*time.Second); err != nil {
		t.Fatalf("daemon did not come up: %v", err)
	}
	// WaitForSocket only proves the socket is listening, and the kernel
	// completes that dial before the daemon has run its OnStart hooks. A
	// daemon.hello is answered only once the accept loop is running, which is
	// after every hook, so one handshake here means internal/remote's hook has
	// already installed its manager and cannot overwrite the stand-ins below.
	hello, err := client.Dial(ctx, client.ClientInfo{Name: "cli", Version: "test", Socket: socket})
	if err != nil {
		t.Fatalf("daemon did not answer hello: %v", err)
	}
	_ = hello.Close()

	// After the hooks have run, not before: internal/remote's OnStart hook
	// installs the real manager as both the remote rows and the router, and
	// this test's stand-ins have to be what is in place when the CLI calls.
	if remote != nil {
		loop.SetRemote(remote)
	}
	daemon.SetRouter(router)
	t.Cleanup(func() { daemon.SetRouter(nil) })

	dial := func(ctx context.Context) (*client.Client, error) {
		return client.Dial(ctx, client.ClientInfo{Name: "cli", Version: "test", Socket: socket})
	}
	prevDial, prevWrite := dialDaemon, connectForWrite
	dialDaemon, connectForWrite = dial, dial
	t.Cleanup(func() { dialDaemon, connectForWrite = prevDial, prevWrite })
}

// onHost points the current command at a host for one test.
func onHost(t *testing.T, name string) {
	t.Helper()
	prev := remoteHostFlag
	remoteHostFlag = name
	t.Cleanup(func() { remoteHostFlag = prev })
}

// TestKillHostGoesToTheRemoteDaemon: `sonar kill 3000 --host fake` selects its
// target from the remote's port table and kills it there. Nothing on this
// machine is touched — the local daemon never even scans for it.
func TestKillHostGoesToTheRemoteDaemon(t *testing.T) {
	resetRouting(t)
	setKillFlags(t, false, true)
	onHost(t, "fake")

	router := &fakeHostRouter{
		name: "fake",
		replies: map[string]any{
			"ports.list": rpc.PortsListResult{Ports: []state.Port{{
				Host: state.LocalhostName, Port: 3000, BindAddress: "127.0.0.1",
				PID: 4242, Process: "node", DisplayName: "api",
			}}},
			"ports.kill": rpc.KillEnvelope{
				MutationResult: rpc.MutationResult{OK: true, Affected: []string{"3000:127.0.0.1"}},
				Results: []state.KillResult{{
					Host: state.LocalhostName, Port: 3000, BindAddress: "127.0.0.1",
					PID: 4242, Name: "api", Method: state.MethodSIGTERM, OK: true,
				}},
			},
		},
	}
	startDaemonForHostTests(t, router, nil)

	out, err := runKillCmd(t, "3000")
	if err != nil {
		t.Fatalf("kill --host: %v\n%s", err, out)
	}

	list := router.sawMethod("ports.list")
	if list == nil {
		t.Fatal("the target was not selected from the remote's port table")
	}
	if list.host != "fake" {
		t.Errorf("ports.list went to %q, want fake", list.host)
	}
	kill := router.sawMethod("ports.kill")
	if kill == nil {
		t.Fatal("ports.kill never reached the remote host")
	}
	if kill.host != "fake" {
		t.Errorf("ports.kill went to %q, want fake", kill.host)
	}
	// The host named the machine; it must not also travel as a param the far
	// side would have to ignore.
	if strings.Contains(kill.params, `"host"`) {
		t.Errorf("forwarded params still carry a host: %s", kill.params)
	}
	if !strings.Contains(kill.params, `"port":3000`) {
		t.Errorf("forwarded params = %s, want the selector", kill.params)
	}

	// The row the user sees is attributed to the host it happened on, and the
	// key is the one the state stream uses for that row.
	var rows []state.KillResult
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not the result list: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Host != "fake" {
		t.Fatalf("result = %+v, want one row tagged fake", rows)
	}
}

// A write to another machine has nowhere to fall back to, and says so.
func TestKillHostRefusesWithoutTheDaemon(t *testing.T) {
	resetRouting(t)
	setKillFlags(t, false, false)
	onHost(t, "fake")

	prev := noDaemonFlag
	noDaemonFlag = true
	t.Cleanup(func() { noDaemonFlag = prev })

	_, err := runKillCmd(t, "3000")
	if err == nil {
		t.Fatal("want an error: --host needs the daemon that holds the bridge")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("err = %v, want it to name the flag", err)
	}
}

// TestListEveryHostShowsAHostColumn is `sonar list --host "*"`: every machine
// in one table, with a HOST column that appears because there is more than one.
func TestListEveryHostShowsAHostColumn(t *testing.T) {
	resetRouting(t)
	display.NoColor = true

	remote := func() state.Rows {
		return state.Rows{
			Ports: []state.Port{{
				Host: "fake", Port: 4321, BindAddress: "0.0.0.0",
				PID: 77, Process: "postgres", DisplayName: "postgres",
				Type: state.TypeUser, IPVersion: "IPv4",
			}},
			Hosts: []state.Host{{Name: "fake", Address: "deploy@box", Status: state.HostConnected}},
		}
	}
	startDaemonForHostTests(t, &fakeHostRouter{name: "fake"}, remote)

	prevHost, prevCols := hostFlag, columnsFlag
	hostFlag, columnsFlag = allHosts, "port,process"
	t.Cleanup(func() { hostFlag, columnsFlag = prevHost, prevCols })

	rows, _, err := listPorts(context.Background(), listQuery{showApps: true})
	if err != nil {
		t.Fatalf(`list --host "*": %v`, err)
	}

	var sawRemote, sawLocal bool
	for _, r := range rows {
		switch r.Host {
		case "fake":
			sawRemote = true
		case state.LocalhostName:
			sawLocal = true
		}
	}
	if !sawRemote {
		t.Fatalf(`--host "*" did not include the remote host's rows (%d rows, none tagged fake)`, len(rows))
	}
	if !sawLocal && len(rows) > 1 {
		t.Error(`--host "*" must include this machine too`)
	}

	out := captureStdout(t, func() {
		display.RenderTable(os.Stdout, rows, display.TableOptions{Columns: []string{"port", "process"}})
	})
	if !strings.Contains(out, "HOST") {
		t.Errorf("the table has no HOST column even though the rows span hosts:\n%s", out)
	}
	if !strings.Contains(out, "fake") {
		t.Errorf("the table does not name the remote host:\n%s", out)
	}
}

// One host, no column: a HOST that says the same thing on every row is noise.
func TestOneHostGetsNoHostColumn(t *testing.T) {
	display.NoColor = true
	rows := []ports.ListeningPort{
		{Host: state.LocalhostName, Port: 3000, Process: "node"},
		{Host: state.LocalhostName, Port: 5432, Process: "postgres"},
	}
	out := captureStdout(t, func() {
		display.RenderTable(os.Stdout, rows, display.TableOptions{Columns: []string{"port", "process"}})
	})
	if strings.Contains(out, "HOST") {
		t.Errorf("a single-host table must not carry a HOST column:\n%s", out)
	}
}
