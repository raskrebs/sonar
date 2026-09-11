package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
)

// fakeDocker is a scripted `docker ps` for a Watcher under test.
type fakeDocker struct {
	mu    sync.Mutex
	calls int
	stats int
	// next is what the next `docker ps` returns.
	containers []container
	err        error
}

func (f *fakeDocker) list(context.Context) ([]container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.containers, f.err
}

func (f *fakeDocker) set(cs []container, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers, f.err = cs, err
}

func (f *fakeDocker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// clock is a manual clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// harness is a Watcher wired to a fake docker, a manual clock and a captured
// debug log. step does what one pass of Run does: refresh if due.
type harness struct {
	w       *Watcher
	docker  *fakeDocker
	clock   *clock
	log     *bytes.Buffer
	changes int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		docker: &fakeDocker{},
		clock:  &clock{t: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)},
		log:    &bytes.Buffer{},
	}
	logger := slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.w = NewWatcher(WatcherOptions{
		Interval: 2 * time.Second,
		Logger:   logger,
		OnChange: func() { h.changes++ },
		Now:      h.clock.now,
	})
	h.w.lookPath = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	h.w.list = h.docker.list
	h.w.stats = func(cs []container) map[string]*ContainerStats {
		h.docker.stats++
		out := map[string]*ContainerStats{}
		for _, c := range cs {
			out[c.name] = &ContainerStats{CPUPercent: 1.5, State: "running"}
		}
		return out
	}
	return h
}

// step runs one refresh if one is due and reports whether it did.
func (h *harness) step() bool {
	if !h.w.due() {
		return false
	}
	h.w.refresh(context.Background())
	return true
}

var web = container{
	name: "shop-web-1", image: "nginx:1.27",
	portMappings:   []portMapping{{hostPort: 8080, containerPort: 80}},
	composeService: "web", composeProject: "shop", composeWorkingDir: "/src/shop",
}

func TestBackoffScheduleDoublesToTheCap(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for i, w := range want {
		if got := backoffFor(i + 1); got != w {
			t.Errorf("backoffFor(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := backoffFor(1000); got != MaxBackoff {
		t.Errorf("backoffFor(1000) = %s, want the cap", got)
	}
}

// A failing Docker is asked again only once each backoff step has passed,
// however often scans poke, and one success resets the schedule.
func TestFailingDockerBacksOffAndASuccessResets(t *testing.T) {
	h := newHarness(t)
	h.docker.set(nil, errors.New("Cannot connect to the Docker daemon"))

	if !h.step() {
		t.Fatal("the first refresh did not run")
	}
	for n := 1; n <= 8; n++ {
		wait := backoffFor(n)
		// Scans poke every second meanwhile; none of them may reach Docker.
		for elapsed := time.Second; elapsed < wait; elapsed += time.Second {
			h.clock.advance(time.Second)
			h.w.Enrich([]ports.ListeningPort{{Port: 9000 + int(elapsed/time.Second)}})
			if h.step() {
				t.Fatalf("after failure %d a refresh ran %s in, before the %s backoff", n, elapsed, wait)
			}
		}
		h.clock.advance(time.Second)
		if !h.step() {
			t.Fatalf("after failure %d no refresh ran once the %s backoff was up", n, wait)
		}
	}
	if got := h.docker.count(); got != 9 {
		t.Errorf("docker ps ran %d times, want 9", got)
	}

	h.docker.set([]container{web}, nil)
	h.clock.advance(MaxBackoff)
	if !h.step() {
		t.Fatal("no refresh after the cap")
	}
	if !h.w.Healthy() {
		t.Error("a successful docker ps did not mark the watcher healthy")
	}
	// Reset: the next refresh is one interval away, not a backoff step.
	h.clock.advance(2 * time.Second)
	if !h.step() {
		t.Error("after a success the watcher still waited out a backoff")
	}
}

// While Docker fails, scans keep enriching from the last list that answered.
func TestFailingDockerServesTheLastGoodList(t *testing.T) {
	h := newHarness(t)
	h.docker.set([]container{web}, nil)
	h.step()

	h.docker.set(nil, errors.New("boom"))
	h.clock.advance(2 * time.Second)
	h.step()
	if h.w.Healthy() {
		t.Fatal("a failed docker ps left the watcher healthy")
	}

	pp := []ports.ListeningPort{{Port: 8080, Type: ports.PortTypeUser}}
	h.w.Enrich(pp)
	if pp[0].Type != ports.PortTypeDocker || pp[0].DockerContainer != "shop-web-1" ||
		pp[0].DockerComposeProject != "shop" || pp[0].DockerComposeWorkingDir != "/src/shop" {
		t.Errorf("port 8080 = %+v, want it enriched from the last good list", pp[0])
	}
}

// No docker on PATH: never exec, never back off, say so once.
func TestNoDockerInstalledStaysSilentAndCheap(t *testing.T) {
	h := newHarness(t)
	h.w.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	for i := 0; i < 50; i++ {
		h.w.Enrich([]ports.ListeningPort{{Port: 3000 + i}})
		h.step()
		h.clock.advance(2 * time.Second)
	}
	if got := h.docker.count(); got != 0 {
		t.Errorf("docker ps ran %d times with no docker installed", got)
	}
	if !h.w.retryAt.IsZero() || h.w.failures != 0 {
		t.Errorf("not installed was treated as a failure: failures=%d retryAt=%s", h.w.failures, h.w.retryAt)
	}
	if n := strings.Count(h.log.String(), "\n"); n != 1 {
		t.Errorf("logged %d lines, want exactly one:\n%s", n, h.log.String())
	}
}

// The debug log names the transitions, not every tick.
func TestLogsOnlyOnHealthChanges(t *testing.T) {
	h := newHarness(t)
	h.docker.set([]container{web}, nil)
	h.step()
	h.clock.advance(2 * time.Second)
	h.step()
	if h.log.Len() != 0 {
		t.Fatalf("healthy refreshes logged:\n%s", h.log.String())
	}

	h.docker.set(nil, errors.New("`docker ps` did not answer within 5s"))
	for i := 0; i < 4; i++ {
		h.clock.advance(MaxBackoff)
		h.step()
	}
	out := h.log.String()
	if n := strings.Count(out, "docker is not answering"); n != 1 {
		t.Errorf("logged the failure %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "did not answer within 5s") {
		t.Errorf("the failure line does not carry the error:\n%s", out)
	}

	h.docker.set([]container{web}, nil)
	h.clock.advance(MaxBackoff)
	h.step()
	h.clock.advance(2 * time.Second)
	h.step()
	if n := strings.Count(h.log.String(), "docker is answering again"); n != 1 {
		t.Errorf("logged the recovery %d times, want once:\n%s", n, h.log.String())
	}
}

// Routine refreshes are rate-limited to the interval; a port nobody knew about
// asks for one early, a port already known or already published does not.
func TestANewUnattributedPortAsksForAnEarlyRefresh(t *testing.T) {
	h := newHarness(t)
	h.docker.set([]container{web}, nil)
	h.w.Enrich([]ports.ListeningPort{{Port: 3000}, {Port: 8080}})
	h.step()

	h.clock.advance(500 * time.Millisecond)
	h.w.Enrich([]ports.ListeningPort{{Port: 3000}, {Port: 8080}})
	if h.step() {
		t.Error("an unchanged port set refreshed before the interval")
	}

	h.clock.advance(100 * time.Millisecond)
	h.w.Enrich([]ports.ListeningPort{{Port: 3000}, {Port: 8080}, {Port: 5432}})
	if !h.step() {
		t.Error("a new port no container publishes did not ask for an early refresh")
	}

	// A new container arrives with the port: the change wakes the loop.
	before := h.changes
	db := container{name: "shop-db-1", portMappings: []portMapping{{hostPort: 5433, containerPort: 5432}}}
	h.docker.set([]container{web, db}, nil)
	h.clock.advance(100 * time.Millisecond)
	h.w.Enrich([]ports.ListeningPort{{Port: 3000}, {Port: 8080}, {Port: 5432}, {Port: 5433}})
	if !h.step() {
		t.Fatal("no early refresh for port 5433")
	}
	if h.changes != before+1 {
		t.Errorf("OnChange ran %d times for one changed list, want 1", h.changes-before)
	}

	// Now 5433 is published by a known container; seeing it again is not news.
	h.clock.advance(100 * time.Millisecond)
	h.w.Enrich([]ports.ListeningPort{{Port: 3000}, {Port: 8080}, {Port: 5432}, {Port: 5433}})
	if h.step() {
		t.Error("a known port asked for an early refresh")
	}

	// An unchanged list does not wake anyone.
	h.clock.advance(2 * time.Second)
	h.step()
	if h.changes != before+1 {
		t.Error("an unchanged list called OnChange")
	}
}

// Stats are only collected once a scan asks, and dropped while Docker fails.
func TestStatsAreCollectedOnDemandAndDroppedOnFailure(t *testing.T) {
	h := newHarness(t)
	h.docker.set([]container{web}, nil)
	h.step()
	if h.docker.stats != 0 {
		t.Fatal("stats were collected before anyone asked")
	}
	if got := h.w.Stats(); got != nil {
		t.Errorf("stats before the first collection = %v, want nil", got)
	}
	h.clock.advance(100 * time.Millisecond)
	if !h.step() {
		t.Fatal("the first Stats call did not ask for a refresh")
	}
	st := h.w.Stats()
	if st["shop-web-1"] == nil || st["shop-web-1"].State != "running" {
		t.Errorf("stats = %v, want shop-web-1", st)
	}

	h.docker.set(nil, errors.New("boom"))
	h.clock.advance(2 * time.Second)
	h.step()
	if got := h.w.Stats(); got != nil {
		t.Errorf("stats after a failure = %v, want none", got)
	}
}

// The point of the step: a scan never waits on Docker, even while a
// `docker ps` hangs, and the hanging one is cut off at the timeout.
func TestAHangingDockerNeverBlocksAScan(t *testing.T) {
	w := NewWatcher(WatcherOptions{Interval: time.Hour})
	w.lookPath = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	w.timeout = 200 * time.Millisecond
	started := make(chan struct{}, 1)
	w.list = func(ctx context.Context) ([]container, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never asked docker")
	}

	begin := time.Now()
	for i := 0; i < 100; i++ {
		pp := []ports.ListeningPort{{Port: 4000 + i}}
		w.Enrich(pp)
		_ = w.Stats()
	}
	if took := time.Since(begin); took > 100*time.Millisecond {
		t.Errorf("100 scans' worth of Enrich took %s while docker ps hung", took)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		state, err := w.state, w.lastErr
		w.mu.Unlock()
		if state == healthDown {
			if err == nil || !strings.Contains(err.Error(), "did not answer within 200ms") {
				t.Errorf("lastErr = %v, want it to say the call timed out", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hanging docker ps was never cut off")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A healthy docker's `ps` output still enriches a port with the container and
// its compose labels, exactly as the inline path did.
func TestParsePSFeedsEnrichment(t *testing.T) {
	out := "shop-web-1\tnginx:1.27\t0.0.0.0:8080->80/tcp, :::8080->80/tcp\tweb\tshop\t/src/shop\n" +
		"loose\tredis:7\t\t\t\t\n"
	cs := parsePS([]byte(out))
	if len(cs) != 2 {
		t.Fatalf("parsed %d containers, want 2: %+v", len(cs), cs)
	}

	h := newHarness(t)
	h.docker.set(cs, nil)
	h.step()
	pp := []ports.ListeningPort{{Port: 8080, BindAddress: "0.0.0.0"}, {Port: 8080, BindAddress: "::"}, {Port: 3000}}
	h.w.Enrich(pp)
	for _, p := range pp[:2] {
		if p.Type != ports.PortTypeDocker || p.DockerContainer != "shop-web-1" || p.DockerImage != "nginx:1.27" ||
			p.DockerComposeService != "web" || p.DockerComposeProject != "shop" ||
			p.DockerComposeWorkingDir != "/src/shop" || p.DockerContainerPort != 80 {
			t.Errorf("port %s:%d = %+v", p.BindAddress, p.Port, p)
		}
	}
	if pp[2].Type == ports.PortTypeDocker {
		t.Errorf("port 3000 was enriched: %+v", pp[2])
	}
}
