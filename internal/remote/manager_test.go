package remote

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// testTimeout is a hang guard, not a latency assertion: a bridge that never
// connects has to fail the test rather than hang the binary.
const testTimeout = 20 * time.Second

// changeSignal records the manager's OnChange callbacks so a test can wait for
// "the multiplexer republished" instead of sleeping.
type changeSignal struct {
	mu sync.Mutex
	ch chan struct{}
	n  int
}

func newChangeSignal() *changeSignal {
	return &changeSignal{ch: make(chan struct{}, 256)}
}

func (c *changeSignal) fire() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

// waitFor polls the manager's rows until cond holds, driven by OnChange.
func waitFor(t *testing.T, sig *changeSignal, m *Manager, what string, cond func(state.Rows) bool) state.Rows {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		if rows := m.Rows(); cond(rows) {
			return rows
		}
		select {
		case <-sig.ch:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; rows = %+v", what, m.Rows())
		}
	}
}

// newTestManager builds a manager wired to a fake remote daemon, with config
// persistence stubbed out so no test writes to the user's home.
func newTestManager(t *testing.T, hosts ...Host) (*Manager, *fakeDaemon, *changeSignal, []state.Event) {
	t.Helper()
	fake := newFakeDaemon(t)
	sig := newChangeSignal()

	var eventsMu sync.Mutex
	var events []state.Event

	m := NewManager(Options{
		Version:  "test",
		Dial:     fake.dialer(),
		OnChange: sig.fire,
		OnEvent: func(ev state.Event) {
			eventsMu.Lock()
			events = append(events, ev)
			eventsMu.Unlock()
		},
		Save: func([]Host) error { return nil },
	})
	m.Start(context.Background(), hosts)
	t.Cleanup(m.Stop)
	return m, fake, sig, events
}

func portRow(port int, bind string) state.Port {
	return state.Port{
		Host: state.LocalhostName, Port: port, BindAddress: bind,
		Process: "node", DisplayName: "node", Type: state.TypeUser,
		ExposedURLs: []string{}, Shares: []state.Share{},
	}
}

func snapshotWith(pp ...state.Port) state.Snapshot {
	return state.Snapshot{
		Ports:    pp,
		Groups:   []state.Group{{Host: state.LocalhostName, Name: "api", Status: "running", Members: []int{pp[0].Port}}},
		Shares:   []state.Share{},
		Proxies:  []state.Proxy{},
		Sessions: []state.SessionRecord{},
		Hosts: []state.Host{{
			Name: state.LocalhostName, Address: state.LocalhostName,
			Status: state.HostConnected, OS: "linux", Arch: "amd64",
			Load: []float64{0.5, 0.4, 0.3},
		}},
	}
}

// TestRemoteRowsAreTaggedAndPrefixed is the multiplexer's core contract: every
// row a remote host contributes carries that host's name, and its delta key is
// namespaced so it cannot collide with a local row on the same port.
func TestRemoteRowsAreTaggedAndPrefixed(t *testing.T) {
	m, fake, sig, _ := newTestManager(t, Host{Name: "hetzner", Target: "deploy@box"})
	fake.waitSubscribed(t)
	fake.setSnapshot(snapshotWith(portRow(3000, "127.0.0.1")))

	rows := waitFor(t, sig, m, "the remote port to arrive", func(r state.Rows) bool {
		return len(r.Ports) == 1
	})

	got := rows.Ports[0]
	if got.Host != "hetzner" {
		t.Errorf("port host = %q, want hetzner", got.Host)
	}
	if want := "hetzner/3000:127.0.0.1"; got.Key() != want {
		t.Errorf("port key = %q, want %q", got.Key(), want)
	}
	if len(rows.Groups) != 1 || rows.Groups[0].Host != "hetzner" {
		t.Errorf("groups = %+v, want one tagged with hetzner", rows.Groups)
	}
	if want := "hetzner/api"; rows.Groups[0].Key() != want {
		t.Errorf("group key = %q, want %q", rows.Groups[0].Key(), want)
	}
	if len(rows.Hosts) != 1 || rows.Hosts[0].Name != "hetzner" {
		t.Fatalf("hosts = %+v, want the remote's row renamed to hetzner", rows.Hosts)
	}
	if h := rows.Hosts[0]; h.Address != "deploy@box" || h.Status != state.HostConnected {
		t.Errorf("host row = %+v, want the ssh target and connected", h)
	}
	if h := rows.Hosts[0]; h.OS != "linux" || len(h.Load) != 3 {
		t.Errorf("host row = %+v, want the remote's own load copied in", h)
	}
	if h := rows.Hosts[0]; h.DaemonVersion != "1.2.3" {
		t.Errorf("daemon_version = %q, want the remote's", h.DaemonVersion)
	}
}

// TestLocalKeysAreUnprefixed guards decision 1: only remote rows are
// namespaced, so a client written before remote hosts existed keeps working.
func TestLocalKeysAreUnprefixed(t *testing.T) {
	local := portRow(3000, "127.0.0.1")
	if want := "3000:127.0.0.1"; local.Key() != want {
		t.Errorf("local key = %q, want %q", local.Key(), want)
	}
	blank := local
	blank.Host = ""
	if want := "3000:127.0.0.1"; blank.Key() != want {
		t.Errorf("key of a row with no host = %q, want %q", blank.Key(), want)
	}
}

// TestTwoHostsDoNotCollide is why the prefix exists: the same port number on
// two machines has to be two rows.
func TestTwoHostsDoNotCollide(t *testing.T) {
	a := portRow(3000, "127.0.0.1")
	a.Host = "alpha"
	b := portRow(3000, "127.0.0.1")
	b.Host = "beta"
	if a.Key() == b.Key() {
		t.Fatalf("both hosts key as %q", a.Key())
	}

	prev := state.Snapshot{Ports: []state.Port{a}}
	next := state.Snapshot{Ports: []state.Port{a, b}}
	d := state.Diff(prev, next)
	if len(d.Ports.Added) != 1 || d.Ports.Added[0].Host != "beta" {
		t.Errorf("delta added = %+v, want only beta's row", d.Ports.Added)
	}
	if len(d.Ports.Removed) != 0 {
		t.Errorf("delta removed = %v, want nothing removed", d.Ports.Removed)
	}
}

// TestRemoveDropsEveryRow: unregistering a host must take its ports, groups
// and its own Host row out of the published state, not just stop updating them.
func TestRemoveDropsEveryRow(t *testing.T) {
	m, fake, sig, _ := newTestManager(t, Host{Name: "hetzner", Target: "deploy@box"})
	fake.waitSubscribed(t)
	fake.setSnapshot(snapshotWith(portRow(3000, "127.0.0.1")))
	waitFor(t, sig, m, "the remote port to arrive", func(r state.Rows) bool { return len(r.Ports) == 1 })

	if err := m.Remove("hetzner"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	rows := m.Rows()
	if len(rows.Ports) != 0 || len(rows.Groups) != 0 || len(rows.Hosts) != 0 {
		t.Fatalf("rows after remove = %+v, want nothing", rows)
	}
	if m.Has("hetzner") {
		t.Error("the host is still registered")
	}
	if err := m.Remove("hetzner"); err == nil {
		t.Error("removing an unknown host should fail")
	}
}

// TestDisconnectDropsRowsButKeepsTheHost: a machine we cannot see contributes
// no ports — showing the last ones would be a lie — but it keeps its row in
// the hosts collection so a client can say why.
func TestDisconnectDropsRowsButKeepsTheHost(t *testing.T) {
	m, fake, sig, _ := newTestManager(t, Host{Name: "hetzner", Target: "deploy@box"})
	fake.waitSubscribed(t)
	fake.setSnapshot(snapshotWith(portRow(3000, "127.0.0.1")))
	waitFor(t, sig, m, "the remote port to arrive", func(r state.Rows) bool { return len(r.Ports) == 1 })

	fake.hangUp()

	rows := waitFor(t, sig, m, "the host to go unreachable", func(r state.Rows) bool {
		return len(r.Ports) == 0 && len(r.Hosts) == 1 && r.Hosts[0].Status != state.HostConnected
	})
	if rows.Hosts[0].Name != "hetzner" {
		t.Errorf("host row = %+v, want hetzner to stay listed", rows.Hosts[0])
	}
}

// TestReconnectRestoresRowsAndResetsBackoff: after a drop the bridge dials
// again from the minimum delay and the host's rows come back.
func TestReconnectRestoresRowsAndResetsBackoff(t *testing.T) {
	m, fake, sig, _ := newTestManager(t, Host{Name: "hetzner", Target: "deploy@box"})
	fake.waitSubscribed(t)
	fake.setSnapshot(snapshotWith(portRow(3000, "127.0.0.1")))
	waitFor(t, sig, m, "the remote port to arrive", func(r state.Rows) bool { return len(r.Ports) == 1 })

	fake.hangUp()
	waitFor(t, sig, m, "the host to go unreachable", func(r state.Rows) bool { return len(r.Ports) == 0 })

	// The dialer makes a fresh session per dial, so the bridge reconnecting is
	// a second accepted connection.
	fake.waitSubscribed(t)
	fake.setSnapshot(snapshotWith(portRow(4000, "127.0.0.1")))

	rows := waitFor(t, sig, m, "the reconnected host's rows", func(r state.Rows) bool {
		return len(r.Ports) == 1 && r.Ports[0].Port == 4000
	})
	if rows.Ports[0].Host != "hetzner" {
		t.Errorf("reconnected port host = %q, want hetzner", rows.Ports[0].Host)
	}
	if rows.Hosts[0].Status != state.HostConnected {
		t.Errorf("status = %q, want connected again", rows.Hosts[0].Status)
	}
}

// TestBackoffProgression pins the spec's 1 s → 30 s curve.
func TestBackoffProgression(t *testing.T) {
	d := ReconnectMin
	seen := []time.Duration{d}
	for i := 0; i < 8; i++ {
		d = nextBackoff(d)
		seen = append(seen, d)
	}
	if seen[0] != time.Second {
		t.Errorf("first delay = %s, want 1s", seen[0])
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("backoff went backwards: %v", seen)
		}
		if seen[i] > ReconnectMax {
			t.Fatalf("backoff exceeded the cap: %v", seen)
		}
	}
	if seen[len(seen)-1] != ReconnectMax {
		t.Errorf("backoff settled at %s, want the %s cap", seen[len(seen)-1], ReconnectMax)
	}
}

// TestProtocolMismatchIsIncompatible: a remote daemon speaking another major
// version is a status, not a crash loop with a useless reason.
func TestProtocolMismatchIsIncompatible(t *testing.T) {
	fake := newFakeDaemon(t)
	fake.protocolVersion = "99.0.0"
	sig := newChangeSignal()

	m := NewManager(Options{
		Version: "test", Dial: fake.dialer(), OnChange: sig.fire,
		Save: func([]Host) error { return nil },
	})
	m.Start(context.Background(), []Host{{Name: "hetzner", Target: "deploy@box"}})
	t.Cleanup(m.Stop)

	rows := waitFor(t, sig, m, "the incompatible status", func(r state.Rows) bool {
		return len(r.Hosts) == 1 && r.Hosts[0].Status == state.HostIncompatible
	})
	if rows.Hosts[0].StatusReason == nil || *rows.Hosts[0].StatusReason == "" {
		t.Error("an incompatible host should say why")
	}
}

// TestCallForwardsVerbatim is decision 3 on the wire: the local daemon hands
// the params over unchanged and returns the remote's result as it came.
func TestCallForwardsVerbatim(t *testing.T) {
	m, fake, _, _ := newTestManager(t, Host{Name: "hetzner", Target: "deploy@box"})

	var gotParams json.RawMessage
	fake.handle("ports.rename", func(params json.RawMessage) (any, *rpc.Error) {
		gotParams = append(json.RawMessage{}, params...)
		return map[string]any{"ok": true, "key": "3000:127.0.0.1", "affected": []string{"3000:127.0.0.1"}}, nil
	})
	fake.waitSubscribed(t)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	sent := json.RawMessage(`{"port":3000,"name":"api"}`)
	out, err := m.Call(ctx, "hetzner", "ports.rename", sent)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(gotParams) != string(sent) {
		t.Errorf("remote saw params %s, want %s", gotParams, sent)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["key"] != "3000:127.0.0.1" {
		t.Errorf("result = %s, want the remote's own reply", out)
	}
}

// TestCallOnAnUnknownHostFails keeps a typo from looking like an empty result.
func TestCallOnAnUnknownHostFails(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	_, err := m.Call(context.Background(), "nope", "ports.list", nil)
	if err == nil {
		t.Fatal("want an error for an unregistered host")
	}
}

// TestAddRejectsDuplicates: the name is the key of everything the host
// contributes, so two hosts cannot share one.
func TestAddRejectsDuplicates(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	h := Host{Name: "hetzner", Target: "deploy@box"}
	if err := m.Add(h); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(h); err == nil {
		t.Fatal("want a duplicate error")
	}
	if got := m.Configs(); len(got) != 1 {
		t.Errorf("configs = %+v, want one host", got)
	}
}

// TestAddPersistsTheHostList proves `remote.add` writes what `sonar serve`
// will read on its next start.
func TestAddPersistsTheHostList(t *testing.T) {
	fake := newFakeDaemon(t)
	var saved [][]Host
	m := NewManager(Options{
		Version: "test", Dial: fake.dialer(),
		Save: func(h []Host) error {
			saved = append(saved, append([]Host{}, h...))
			return nil
		},
	})
	m.Start(context.Background(), nil)
	t.Cleanup(m.Stop)

	if err := m.Add(Host{Name: "a", Target: "me@a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Host{Name: "b", Target: "me@b", SSHArgs: []string{"-J", "bastion"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("a"); err != nil {
		t.Fatal(err)
	}

	if len(saved) != 3 {
		t.Fatalf("saved %d times, want 3", len(saved))
	}
	if len(saved[2]) != 1 || saved[2][0].Name != "b" {
		t.Errorf("final saved list = %+v, want only b", saved[2])
	}
	if len(saved[1]) != 2 || len(saved[1][1].SSHArgs) != 2 {
		t.Errorf("saved list = %+v, want b's ssh_args preserved", saved[1])
	}
}

// TestEventsAreForwardedWithTheHost: a remote port_up has to say which machine
// it happened on, or a client cannot place it.
func TestEventsAreForwardedWithTheHost(t *testing.T) {
	fake := newFakeDaemon(t)
	sig := newChangeSignal()
	got := make(chan state.Event, 8)

	m := NewManager(Options{
		Version: "test", Dial: fake.dialer(), OnChange: sig.fire,
		OnEvent: func(ev state.Event) { got <- ev },
		Save:    func([]Host) error { return nil },
	})
	m.Start(context.Background(), []Host{{Name: "hetzner", Target: "deploy@box"}})
	t.Cleanup(m.Stop)
	fake.waitSubscribed(t)

	p := portRow(3000, "127.0.0.1")
	fake.pushEvent(state.Event{Kind: "port_up", At: "2026-09-06T10:00:01Z", Port: &p})

	select {
	case ev := <-got:
		if ev.Host != "hetzner" {
			t.Errorf("event host = %q, want hetzner", ev.Host)
		}
		if ev.Port == nil || ev.Port.Host != "hetzner" {
			t.Errorf("event port = %+v, want the port tagged too", ev.Port)
		}
	case <-time.After(testTimeout):
		t.Fatal("the event never arrived")
	}
}

// TestNormalizeHost covers the naming rules the spec fixes.
func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		name    string
		in      Host
		want    string
		wantErr bool
	}{
		{name: "derives the name from the target", in: Host{Target: "deploy@203.0.113.7"}, want: "203-0-113-7"},
		{name: "keeps an explicit name", in: Host{Name: "hetzner", Target: "deploy@box"}, want: "hetzner"},
		{name: "rejects localhost", in: Host{Name: "localhost", Target: "me@box"}, wantErr: true},
		{name: "rejects an empty target", in: Host{Name: "a"}, wantErr: true},
		{name: "rejects upper case", in: Host{Name: "Hetzner", Target: "me@box"}, wantErr: true},
		{name: "rejects a bad port", in: Host{Name: "a", Target: "me@box", Port: 70000}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeHost(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.want {
				t.Errorf("name = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

// TestSSHArgs pins the command line the bridge asks ssh to run.
func TestSSHArgs(t *testing.T) {
	got := SSHArgs(Host{
		Name: "hetzner", Target: "deploy@box", Port: 2222,
		Identity: "/keys/id", SSHArgs: []string{"-J", "bastion"},
		RemoteBin: "~/.local/bin/sonar",
	})
	want := []string{
		"-o", "BatchMode=yes", "-p", "2222", "-i", "/keys/id",
		"-J", "bastion", "deploy@box", "~/.local/bin/sonar daemon stdio",
	}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}

	bare := SSHArgs(Host{Name: "a", Target: "box"})
	const def = `sh -c 'PATH="$HOME/.local/bin:$PATH"; exec sonar daemon stdio'`
	if bare[len(bare)-1] != def {
		t.Errorf("remote command = %q, want %q", bare[len(bare)-1], def)
	}

	// A named binary is run as written, with no sh around it: that is what a
	// Windows host, whose shell is not a POSIX one, relies on.
	if got, want := FarCommand("sonar", "--no-autostart"), "sonar daemon stdio --no-autostart"; got != want {
		t.Errorf("FarCommand with a named binary = %q, want %q", got, want)
	}
}

// TestFarCommandFindsAnInstallOffThePATH runs the default command the way the
// far side does — in a shell whose PATH has no ~/.local/bin, which is what ssh
// gives a command on a stock Debian or Ubuntu — and expects it to reach the
// sonar `sonar remote install` put there.
func TestFarCommandFindsAnInstallOffThePATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the far side is a POSIX shell")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho reached \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sonar"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(command string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if out, err := run(DefaultRemoteBinary + " " + RemoteCommand); err == nil {
		t.Fatalf("a bare sonar ran (%q): this PATH was meant not to have it", out)
	}
	out, err := run(FarCommand("", "--no-autostart"))
	if err != nil {
		t.Fatalf("FarCommand: %v: %s", err, out)
	}
	if out != "reached daemon stdio --no-autostart" {
		t.Errorf("output = %q, want the install's sonar to have run", out)
	}
}

// TestTargetOfAcceptsHostAsAnAlias: some clients call the SSH destination
// "host". Both spellings mean the string ssh receives.
func TestTargetOfAcceptsHostAsAnAlias(t *testing.T) {
	tests := []struct {
		name string
		in   rpc.RemoteAddParams
		want string
	}{
		{"target only", rpc.RemoteAddParams{Target: "deploy@box"}, "deploy@box"},
		{"host only", rpc.RemoteAddParams{Host: "deploy@box"}, "deploy@box"},
		{"both, target wins", rpc.RemoteAddParams{Target: "a@x", Host: "b@y"}, "a@x"},
		{"neither", rpc.RemoteAddParams{}, ""},
	}
	for _, tc := range tests {
		if got := TargetOf(tc.in); got != tc.want {
			t.Errorf("%s: TargetOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}
