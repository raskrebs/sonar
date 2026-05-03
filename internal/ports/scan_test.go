package ports

import (
	"fmt"
	"testing"
)

func TestParseLsof_MultipleBindsSamePort(t *testing.T) {
	output := `COMMAND   PID  USER   FD   TYPE   DEVICE SIZE/OFF NODE NAME
nginx   12345 root    6u  IPv4  1234567      0t0  TCP 192.168.2.5:80 (LISTEN)
nginx   12345 root    7u  IPv4  1234568      0t0  TCP 172.31.96.5:80 (LISTEN)
nginx   12345 root    8u  IPv4  1234569      0t0  TCP 192.168.4.5:443 (LISTEN)
nginx   12345 root    9u  IPv4  1234570      0t0  TCP 192.168.4.5:80 (LISTEN)
`
	results := parseLsof(output)

	if len(results) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(results))
	}

	// Verify all bind addresses are present
	binds := map[string]bool{}
	for _, r := range results {
		binds[r.BindAddress+":"+itoa(r.Port)] = true
	}
	expected := []string{"192.168.2.5:80", "172.31.96.5:80", "192.168.4.5:443", "192.168.4.5:80"}
	for _, e := range expected {
		if !binds[e] {
			t.Errorf("expected bind %s not found in results", e)
		}
	}
}

func TestParseLsof_DedupsSamePortAndBind(t *testing.T) {
	output := `COMMAND   PID  USER   FD   TYPE   DEVICE SIZE/OFF NODE NAME
node    1234 user    6u  IPv4  1234567      0t0  TCP *:3000 (LISTEN)
node    1234 user    7u  IPv4  1234568      0t0  TCP *:3000 (LISTEN)
`
	results := parseLsof(output)

	if len(results) != 1 {
		t.Fatalf("expected 1 entry (deduped), got %d", len(results))
	}
	if results[0].BindAddress != "0.0.0.0" {
		t.Errorf("expected bind 0.0.0.0, got %s", results[0].BindAddress)
	}
}

func TestParseSS_MultipleBindsSamePort(t *testing.T) {
	output := `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      128    127.0.0.2:8080       0.0.0.0:*     users:(("nc",pid=1001,fd=3))
LISTEN 0      128    127.0.0.3:8080       0.0.0.0:*     users:(("nc",pid=1002,fd=3))
LISTEN 0      128    127.0.0.4:8080       0.0.0.0:*     users:(("nc",pid=1003,fd=3))
`
	results := parseSS(output)

	if len(results) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(results))
	}

	addrs := map[string]bool{}
	for _, r := range results {
		addrs[r.BindAddress] = true
	}
	for _, addr := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4"} {
		if !addrs[addr] {
			t.Errorf("expected bind address %s not found", addr)
		}
	}
}

func TestParseNetstat_MultipleBindsSamePort(t *testing.T) {
	output := `  Proto  Local Address          Foreign Address        State           PID
  TCP    192.168.1.1:8080       0.0.0.0:0              LISTENING       1001
  TCP    192.168.1.2:8080       0.0.0.0:0              LISTENING       1002
  TCP    192.168.1.3:8080       0.0.0.0:0              LISTENING       1003
`
	results := parseNetstat(output)

	if len(results) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(results))
	}

	addrs := map[string]bool{}
	for _, r := range results {
		addrs[r.BindAddress] = true
	}
	for _, addr := range []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"} {
		if !addrs[addr] {
			t.Errorf("expected bind address %s not found", addr)
		}
	}
}

func TestPortKey(t *testing.T) {
	lp := ListeningPort{Port: 8080, BindAddress: "127.0.0.1"}
	if lp.PortKey() != "8080:127.0.0.1" {
		t.Errorf("expected '8080:127.0.0.1', got '%s'", lp.PortKey())
	}
}

func TestURL_UsesBindAddress(t *testing.T) {
	tests := []struct {
		bind string
		want string
	}{
		{"0.0.0.0", "http://localhost:3000"},
		{"", "http://localhost:3000"},
		{"[::]", "http://localhost:3000"},
		{"192.168.1.5", "http://192.168.1.5:3000"},
		{"127.0.0.1", "http://127.0.0.1:3000"},
	}
	for _, tt := range tests {
		lp := ListeningPort{Port: 3000, BindAddress: tt.bind}
		got := lp.URL()
		if got != tt.want {
			t.Errorf("URL() with bind %q = %q, want %q", tt.bind, got, tt.want)
		}
	}
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
