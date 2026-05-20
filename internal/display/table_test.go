package display

import (
	"testing"

	"github.com/raskrebs/sonar/internal/ports"
)

func TestColorProcessServiceFallback(t *testing.T) {
	NoColor = true

	// Unknown process on a well-known port falls back to the service hint.
	got := colorProcess(ports.ListeningPort{Port: 53})
	if got != "~dns" {
		t.Errorf("colorProcess(port 53, no process) = %q, want %q", got, "~dns")
	}

	// Unknown process on a non-well-known port stays blank.
	got = colorProcess(ports.ListeningPort{Port: 3000})
	if got != "" {
		t.Errorf("colorProcess(port 3000, no process) = %q, want empty", got)
	}

	// A resolved process name is used as-is, not overridden by the hint.
	got = colorProcess(ports.ListeningPort{Port: 53, Process: "dnsmasq"})
	if got != "dnsmasq" {
		t.Errorf("colorProcess(port 53, process dnsmasq) = %q, want %q", got, "dnsmasq")
	}
}
