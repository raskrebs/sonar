package groupstart

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
)

// TestDownOnlyStopsTheNamedServiceAndItsClaim is `sonar down --only api` end
// to end: of two running `port: auto` services, only api goes down and only
// api's claim is released; db keeps running on the port it was given.
func TestDownOnlyStopsTheNamedServiceAndItsClaim(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := serviceCmd(t)
	path := writeConfig(t, dir, fmt.Sprintf(`name: downtest
services:
  - name: db
    cmd: %s
    port: auto
  - name: api
    cmd: %s
    port: auto
    depends_on: [db]
`, cmd, cmd))

	watch := &watchList{}
	c := startDaemonWith(t, ctx, watchScan(dir, watch))

	var start rpc.GroupsStartResult
	s, err := c.Stream(ctx, "groups.start", rpc.GroupsStartParams{ConfigPath: &path}, &start)
	if err != nil {
		t.Fatalf("groups.start: %v", err)
	}
	defer s.Close()
	chunks, end := collectWatching(t, s, watch)
	t.Cleanup(func() { killPIDs(chunks) })
	if len(end.Started) != 2 || len(end.Errors) != 0 {
		dumpLogs(t, chunks)
		t.Fatalf("end = %+v, chunks = %+v", end, chunks)
	}
	var dbPort, apiPort int
	for _, ch := range chunks {
		switch ch.Service {
		case "db":
			dbPort = ch.Port
		case "api":
			apiPort = ch.Port
		}
	}
	waitListening(t, dbPort, apiPort)

	// The name carries a space the way `--only "db, api"` hands it over; the
	// claim is released under the trimmed name all the same.
	var env rpc.KillEnvelope
	if err := c.Call(ctx, "groups.kill", rpc.GroupsKillParams{
		ConfigPath: &path, Release: true, Only: []string{" api"},
	}, &env); err != nil {
		t.Fatalf("groups.kill --only api: %v", err)
	}
	for _, r := range env.Results {
		if r.Port == dbPort {
			t.Fatalf("db's port %d was in the kill: %+v", dbPort, env.Results)
		}
	}
	if env.Released != 1 {
		t.Errorf("released = %d, want api's one claim", env.Released)
	}

	deadline := time.Now().Add(10 * time.Second)
	for dialable(apiPort) {
		if time.Now().After(deadline) {
			t.Fatalf("api still listens on %d after the kill", apiPort)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dialable(dbPort) {
		t.Fatalf("db stopped listening on %d; only api was named", dbPort)
	}

	var claims rpc.ClaimsListResult
	if err := c.Call(ctx, "claims.list", struct{}{}, &claims); err != nil {
		t.Fatalf("claims.list: %v", err)
	}
	var keys []string
	for _, cl := range claims.Claims {
		keys = append(keys, cl.Key)
	}
	joined := strings.Join(keys, " ")
	if !strings.Contains(joined, "/db") || strings.Contains(joined, "/api") {
		t.Fatalf("claims after the kill = %v, want db's kept and api's released", keys)
	}
}

// waitListening blocks until every port answers, or fails the test: a started
// service binds a moment after groups.start reports it.
func waitListening(t *testing.T, ports ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, port := range ports {
		for !dialable(port) {
			if time.Now().After(deadline) {
				t.Fatalf("port %d never started listening", port)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
