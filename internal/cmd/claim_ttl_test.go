package cmd

import (
	"testing"
	"time"
)

// --ttl has no default of its own, so the daemon's `claims.ttl` can
// apply; a ttl the user typed is parsed, and a bad one is refused here.
func TestClaimTTLFlagLeavesTheDefaultToTheDaemon(t *testing.T) {
	prev := claimTTLFlag
	t.Cleanup(func() { claimTTLFlag = prev })

	if def := claimCmd.Flags().Lookup("ttl").DefValue; def != "" {
		t.Errorf("--ttl defaults to %q, want no default so the daemon's applies", def)
	}
	claimTTLFlag = " 2h "
	if ttl, err := claimTTL(); err != nil || ttl != 2*time.Hour {
		t.Errorf("--ttl 2h = %s, %v; want 2h", ttl, err)
	}
	for _, bad := range []string{"", "soon", "0", "-5m"} {
		claimTTLFlag = bad
		if _, err := claimTTL(); err == nil {
			t.Errorf("--ttl %q was accepted", bad)
		}
	}
}
