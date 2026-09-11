package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/state"
)

func hostFixture() state.Host {
	uptime := int64(824253)
	cpu := 12.4
	memUsed, memTotal := int64(9)<<30, int64(32)<<30
	diskUsed, diskTotal := int64(412)<<30, int64(931)<<30
	return state.Host{
		Name: state.LocalhostName, Address: state.LocalhostName,
		Status: state.HostConnected, DaemonVersion: "0.5.1", ProtocolVersion: "1.0.0",
		OS: "linux", Arch: "amd64", Kernel: "6.8.0-40-generic",
		UptimeS: &uptime, CPUPercent: &cpu,
		Load:       []float64{1.24, 0.98, 0.71},
		MemoryUsed: &memUsed, MemoryTotal: &memTotal,
		DiskUsed: &diskUsed, DiskTotal: &diskTotal, DiskPath: "/",
		Ports: 6, Groups: 2, LastSeen: "2026-09-06T10:00:00Z",
	}
}

// The golden table. Column widths are the contract with whoever reads this in
// a terminal, so they are pinned rather than eyeballed.
func TestHostTableIsGolden(t *testing.T) {
	windows := hostFixture()
	windows.Name = "hetzner"
	windows.OS, windows.Arch = "windows", "amd64"
	windows.Load = nil // no load average on Windows
	windows.CPUPercent = nil

	var buf bytes.Buffer
	renderHosts(&buf, []state.Host{hostFixture(), windows})

	want := strings.Join([]string{
		"NAME           STATUS       OS/ARCH         UPTIME     CPU     LOAD                MEMORY           DISK",
		"localhost      connected    linux/amd64     9d 12h     12.4%   1.24 0.98 0.71      9.0/32.0 GiB     412.0/931.0 GiB",
		"hetzner        connected    windows/amd64   9d 12h     -       -                   9.0/32.0 GiB     412.0/931.0 GiB",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("host table\n got:\n%s\nwant:\n%s", got, want)
	}
}

// GPUs print one line each under their host: a unified-memory GPU shows what
// it holds, a discrete one used/total, and an unknown reading a dash. A host
// whose GPUs were not collected, or that has none, prints no GPU line at all.
func TestHostTableListsGPUsUnderTheirHost(t *testing.T) {
	util, used := 21.0, int64(1536)<<20
	vramUsed, vramTotal := int64(2)<<30, int64(24)<<30
	mac := hostFixture()
	mac.Name = "mac"
	mac.GPUs = []state.GPU{{Name: "Apple M5 Pro", UtilizationPercent: &util, MemoryUsedBytes: &used}}
	gpuBox := hostFixture()
	gpuBox.Name = "gpubox"
	gpuBox.GPUs = []state.GPU{
		{Name: "NVIDIA GeForce RTX 4090", MemoryUsedBytes: &vramUsed, MemoryTotalBytes: &vramTotal},
	}
	none := hostFixture()
	none.Name = "none"
	none.GPUs = []state.GPU{}

	var buf bytes.Buffer
	renderHosts(&buf, []state.Host{mac, gpuBox, none, hostFixture()})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want header + 4 hosts + 2 GPU lines:\n%s", len(lines), buf.String())
	}
	if want := "  gpu Apple M5 Pro  21%  1.5 GiB in use"; lines[2] != want {
		t.Errorf("unified GPU line = %q, want %q", lines[2], want)
	}
	if want := "  gpu NVIDIA GeForce RTX 4090  -  2.0/24.0 GiB"; lines[4] != want {
		t.Errorf("discrete GPU line = %q, want %q", lines[4], want)
	}
}

func TestHostTableWithNoHosts(t *testing.T) {
	var buf bytes.Buffer
	renderHosts(&buf, nil)
	if got := buf.String(); got != "No hosts.\n" {
		t.Errorf("empty table = %q", got)
	}
}

// A machine whose load could not be read prints dashes, never zeroes: "we do
// not know" and "the machine is idle" are different answers.
func TestHostTableRendersUnknownsAsDashes(t *testing.T) {
	var buf bytes.Buffer
	renderHosts(&buf, []state.Host{{Name: "localhost", Status: state.HostConnected}})
	row := strings.Split(buf.String(), "\n")[1]
	if strings.Count(row, "-") < 4 {
		t.Errorf("row with nothing measured = %q, want dashes for every unknown", row)
	}
}

func TestHostUptimeFormats(t *testing.T) {
	tests := map[int64]string{
		0:      "0s",
		45:     "45s",
		90:     "1m",
		3700:   "1h 1m",
		824253: "9d 12h",
	}
	for secs, want := range tests {
		v := secs
		if got := hostUptime(&v); got != want {
			t.Errorf("hostUptime(%d) = %q, want %q", secs, got, want)
		}
	}
	if got := hostUptime(nil); got != "-" {
		t.Errorf("hostUptime(nil) = %q, want a dash", got)
	}
}

func TestHostUsagePicksAUnitFromTheTotal(t *testing.T) {
	mb := int64(700) << 20
	gb := int64(4) << 30
	if got := hostUsage(&mb, &gb); got != "0.7/4.0 GiB" {
		t.Errorf("hostUsage = %q, want 0.7/4.0 GiB", got)
	}
	if got := hostUsage(nil, &gb); got != "-" {
		t.Errorf("hostUsage with no used figure = %q, want a dash", got)
	}
}

// Host stats live in the daemon, so the command says so rather than printing
// an empty table.
func TestHostCommandWithoutADaemonSaysSo(t *testing.T) {
	prev := dialDaemon
	dialDaemon = func(context.Context) (*client.Client, error) { return nil, client.ErrNotRunning }
	t.Cleanup(func() { dialDaemon = prev })

	_, err := readHosts(context.Background())
	if err == nil {
		t.Fatal("readHosts without a daemon did not fail")
	}
	if !strings.Contains(err.Error(), "sonar serve") {
		t.Errorf("error = %q, want it to name the command that starts a daemon", err)
	}
	if errors.Is(err, client.ErrNotRunning) {
		t.Error("the raw dial error reached the user")
	}
}
