package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/claims"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/ports"
)

func TestClaimsCapabilityIsAdvertised(t *testing.T) {
	for _, c := range Capabilities() {
		if c == "claims" {
			return
		}
	}
	t.Fatalf("capabilities = %v, missing claims", Capabilities())
}

func TestAcquireIsDeterministicAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	want := claims.DefaultRange.Base("myapp/feature-x")

	var first rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{
		Project: "myapp", Worktree: "feature-x",
	}, &first); e != nil {
		t.Fatalf("claims.acquire: %v", e)
	}
	if !first.OK || first.Key != "myapp/feature-x" {
		t.Fatalf("result = %+v", first)
	}
	if len(first.Ports) != 1 || first.Ports[0] != want {
		t.Fatalf("ports = %v, want the derived %d", first.Ports, want)
	}
	if first.ExpiresAt == "" {
		t.Error("expires_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, first.ExpiresAt); err != nil {
		t.Errorf("expires_at %q is not RFC 3339: %v", first.ExpiresAt, err)
	}

	var second rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{
		Key: "myapp/feature-x", TTLSeconds: 60,
	}, &second); e != nil {
		t.Fatalf("second claims.acquire: %v", e)
	}
	if len(second.Ports) != 1 || second.Ports[0] != want {
		t.Fatalf("second acquire = %v, want the same port", second.Ports)
	}
	if second.ExpiresAt >= first.ExpiresAt {
		t.Errorf("expires_at = %s, want the shorter ttl to move it earlier than %s",
			second.ExpiresAt, first.ExpiresAt)
	}
}

func TestAcquireSkipsAListeningPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := claims.DefaultRange.Base("myapp/main")
	h, _ := storeHarness(t, ctx, ports.ListeningPort{
		Port: base, PID: 4242, Process: "python3", Command: "python3 -m http.server",
	})
	c := h.dial(ctx)

	var res rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{Project: "myapp"}, &res); e != nil {
		t.Fatalf("claims.acquire: %v", e)
	}
	if len(res.Ports) != 1 || res.Ports[0] != base+1 {
		t.Fatalf("ports = %v, want %d: the base port is serving", res.Ports, base+1)
	}
}

func TestClaimsListAndRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	for _, key := range []string{"alpha/main", "beta/main"} {
		var res rpc.ClaimsAcquireResult
		if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{Key: key, Count: 2}, &res); e != nil {
			t.Fatalf("claims.acquire %s: %v", key, e)
		}
	}

	var list rpc.ClaimsListResult
	if e := c.call("claims.list", struct{}{}, &list); e != nil {
		t.Fatalf("claims.list: %v", e)
	}
	if len(list.Claims) != 2 {
		t.Fatalf("claims = %+v, want two", list.Claims)
	}
	if list.Claims[0].Key != "alpha/main" || len(list.Claims[0].Ports) != 2 {
		t.Errorf("first claim = %+v", list.Claims[0])
	}
	if list.Claims[0].Project != "alpha" || list.Claims[0].Worktree != "main" {
		t.Errorf("project/worktree were not recovered from the key: %+v", list.Claims[0])
	}

	var rel rpc.ClaimsReleaseResult
	if e := c.call("claims.release", rpc.ClaimsReleaseParams{Key: "alpha/main"}, &rel); e != nil {
		t.Fatalf("claims.release: %v", e)
	}
	if !rel.OK || rel.Released != 2 {
		t.Fatalf("release = %+v, want two ports released", rel)
	}

	if e := c.call("claims.list", struct{}{}, &list); e != nil {
		t.Fatalf("claims.list: %v", e)
	}
	if len(list.Claims) != 1 || list.Claims[0].Key != "beta/main" {
		t.Fatalf("claims after release = %+v, want only beta", list.Claims)
	}
}

func TestReleaseNeedsAKeyAndAcquireNeedsAProject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	if e := c.call("claims.release", rpc.ClaimsReleaseParams{}, nil); e == nil ||
		e.Code != rpc.CodeInvalidParams {
		t.Fatalf("release with no key = %v, want invalid_params", e)
	}
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{}, nil); e == nil ||
		e.Code != rpc.CodeInvalidParams {
		t.Fatalf("acquire with no project = %v, want invalid_params", e)
	}
}

func TestPortsNextSkipsForeignClaimsAndReusesItsOwn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	var claimed rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{Key: "myapp/main"}, &claimed); e != nil {
		t.Fatalf("claims.acquire: %v", e)
	}
	port := claimed.Ports[0]

	// Starting the search at the claimed port: a foreign caller steps over it.
	var next rpc.PortsNextResult
	if e := c.call("ports.next", rpc.PortsNextParams{Start: port}, &next); e != nil {
		t.Fatalf("ports.next: %v", e)
	}
	if len(next.Ports) != 1 || next.Ports[0] == port {
		t.Fatalf("ports.next = %v, want it to skip the claimed %d", next.Ports, port)
	}

	// The key that holds it sees it as free.
	key := "myapp/main"
	if e := c.call("ports.next", rpc.PortsNextParams{Start: port, ClaimKey: &key}, &next); e != nil {
		t.Fatalf("ports.next with claim_key: %v", e)
	}
	if len(next.Ports) != 1 || next.Ports[0] != port {
		t.Fatalf("ports.next with its own claim_key = %v, want %d", next.Ports, port)
	}

	// A released claim stops excluding anything.
	if e := c.call("claims.release", rpc.ClaimsReleaseParams{Key: key}, nil); e != nil {
		t.Fatalf("claims.release: %v", e)
	}
	if e := c.call("ports.next", rpc.PortsNextParams{Start: port}, &next); e != nil {
		t.Fatalf("ports.next after release: %v", e)
	}
	if next.Ports[0] != port {
		t.Fatalf("ports.next after release = %v, want %d back", next.Ports, port)
	}
}

// TestTwoAgentsAskingAtOnceGetDifferentPorts is the step's demo in test form:
// parallel agents, one daemon, no collision.
func TestTwoAgentsAskingAtOnceGetDifferentPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)

	const agents = 8
	conns := make([]*testClient, agents)
	for i := range conns {
		conns[i] = h.dial(ctx)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		got   = map[int]string{}
		fails []string
		start = make(chan struct{})
	)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := "myapp/" + string(rune('a'+i))
			var res rpc.ClaimsAcquireResult
			if e := conns[i].call("claims.acquire", rpc.ClaimsAcquireParams{Key: key}, &res); e != nil {
				mu.Lock()
				fails = append(fails, key+": "+e.Message)
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range res.Ports {
				if prev, dup := got[p]; dup {
					fails = append(fails, "port collision on "+key+" and "+prev)
				}
				got[p] = key
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(fails) > 0 {
		t.Fatalf("%v", fails)
	}
	if len(got) != agents {
		t.Fatalf("handed out %d distinct ports to %d agents", len(got), agents)
	}
}

// TestConfiguredClaimTTLIsTheDefaultForACall is `claims.ttl`: a call that
// names no ttl gets the configured life, a call that names one keeps it, and
// `daemon.status` says which is in effect.
func TestConfiguredClaimTTLIsTheDefaultForACall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	// Set on the runtime rather than through Options: the harness builds its
	// Server itself, and the runtime is where the handlers read it.
	h.srv.runtime.ClaimTTL = 2 * time.Hour
	c := h.dial(ctx)

	var status rpc.DaemonStatusResult
	if e := c.call("daemon.status", rpc.Empty{}, &status); e != nil {
		t.Fatalf("daemon.status: %v", e)
	}
	if status.ClaimTTLMs != int((2 * time.Hour).Milliseconds()) {
		t.Errorf("claim_ttl_ms = %d, want two hours", status.ClaimTTLMs)
	}

	before := time.Now()
	var res rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{Project: "app", Worktree: "wt"}, &res); e != nil {
		t.Fatalf("claims.acquire: %v", e)
	}
	expires, err := time.Parse(time.RFC3339, res.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", res.ExpiresAt, err)
	}
	if life := expires.Sub(before); life < 2*time.Hour-time.Minute || life > 2*time.Hour+time.Minute {
		t.Errorf("claim lives %s, want about the configured two hours", life)
	}

	var short rpc.ClaimsAcquireResult
	if e := c.call("claims.acquire", rpc.ClaimsAcquireParams{Key: res.Key, TTLSeconds: 60}, &short); e != nil {
		t.Fatalf("claims.acquire with ttl: %v", e)
	}
	if short.ExpiresAt >= res.ExpiresAt {
		t.Errorf("an explicit ttl should win over the configured one: %s is not before %s",
			short.ExpiresAt, res.ExpiresAt)
	}
}

func TestUnsetClaimTTLReportsTheBuiltInDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := storeHarness(t, ctx)
	c := h.dial(ctx)

	var status rpc.DaemonStatusResult
	if e := c.call("daemon.status", rpc.Empty{}, &status); e != nil {
		t.Fatalf("daemon.status: %v", e)
	}
	if status.ClaimTTLMs != int(claims.DefaultTTL.Milliseconds()) {
		t.Errorf("claim_ttl_ms = %d, want the built-in %s", status.ClaimTTLMs, claims.DefaultTTL)
	}
}
