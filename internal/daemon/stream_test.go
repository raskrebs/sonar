package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// unregisterHandler drops a method again. Only tests need it: RegisterHandler
// panics on a duplicate, so a test that installs a probe has to clean up.
func unregisterHandler(method string) {
	handlersMu.Lock()
	delete(handlers, method)
	handlersMu.Unlock()
}

// streamProbe is a streaming method that emits whatever the test pushes onto
// its channel, so the lifecycle can be driven step by step.
type streamProbe struct {
	lines   chan string
	started chan *Stream
	done    chan error
}

func newStreamProbe() *streamProbe {
	return &streamProbe{
		lines:   make(chan string, 4),
		started: make(chan *Stream, 1),
		done:    make(chan error, 1),
	}
}

func (p *streamProbe) handler(end any) Handler {
	return func(ctx context.Context, req *Request) (any, error) {
		return StartStream(ctx, req, map[string]any{"source": "probe"},
			func(ctx context.Context, s *Stream) (any, error) {
				p.started <- s
				for {
					select {
					case <-ctx.Done():
						p.done <- ctx.Err()
						return end, ctx.Err()
					case line, ok := <-p.lines:
						if !ok {
							p.done <- nil
							return end, nil
						}
						if err := s.Send(map[string]string{"line": line}); err != nil {
							p.done <- err
							return end, err
						}
					}
				}
			})
	}
}

// nextNotification reads until a notification with the wanted method arrives.
func (c *testClient) nextNotification(method string) rpc.Message {
	c.t.Helper()
	for {
		m := c.read()
		if m.IsNotification() && m.Method == method {
			return m
		}
	}
}

func TestStreamChunkEndAndInitialReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newStreamProbe()
	RegisterHandler("test.streamChunks", p.handler(map[string]any{"lines": 2}))
	t.Cleanup(func() { unregisterHandler("test.streamChunks") })

	h := newHarness(t, ctx)
	c := h.dial(ctx)

	var start struct {
		Source         string `json:"source"`
		SubscriptionID string `json:"subscription_id"`
	}
	if e := c.call("test.streamChunks", rpc.Empty{}, &start); e != nil {
		t.Fatalf("starting stream: %v", e)
	}
	if start.Source != "probe" {
		t.Fatalf("initial payload lost: %+v", start)
	}
	if start.SubscriptionID == "" {
		t.Fatal("no subscription_id in the streaming reply")
	}

	p.lines <- "one"
	p.lines <- "two"

	for _, want := range []string{"one", "two"} {
		msg := c.nextNotification(rpc.MethodStreamChunk)
		var chunk rpc.StreamChunk
		if err := json.Unmarshal(msg.Params, &chunk); err != nil {
			t.Fatalf("decoding chunk: %v", err)
		}
		if chunk.ID != start.SubscriptionID {
			t.Fatalf("chunk id = %q, want %q", chunk.ID, start.SubscriptionID)
		}
		var body struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal(chunk.Data, &body); err != nil {
			t.Fatalf("decoding chunk data: %v", err)
		}
		if body.Line != want {
			t.Fatalf("chunk line = %q, want %q", body.Line, want)
		}
	}

	close(p.lines)

	msg := c.nextNotification(rpc.MethodStreamEnd)
	var end rpc.StreamEnd
	if err := json.Unmarshal(msg.Params, &end); err != nil {
		t.Fatalf("decoding stream.end: %v", err)
	}
	if end.ID != start.SubscriptionID {
		t.Fatalf("stream.end id = %q, want %q", end.ID, start.SubscriptionID)
	}
	if end.Error != nil {
		t.Fatalf("stream.end carried an error: %v", end.Error)
	}
	if string(end.Data) != `{"lines":2}` {
		t.Fatalf("stream.end data = %s, want the producer's final payload", end.Data)
	}
}

func TestStreamCancelStopsTheProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newStreamProbe()
	RegisterHandler("test.streamCancel", p.handler(nil))
	t.Cleanup(func() { unregisterHandler("test.streamCancel") })

	h := newHarness(t, ctx)
	c := h.dial(ctx)

	var start rpc.StreamStart
	if e := c.call("test.streamCancel", rpc.Empty{}, &start); e != nil {
		t.Fatalf("starting stream: %v", e)
	}
	<-p.started

	// The acknowledgement and the stream's end are written by two goroutines —
	// the dispatcher and the producer the cancel just stopped — so they arrive
	// in either order. `call` drops every notification it reads on the way to
	// its reply, which would throw the end away whenever it came first and
	// leave the read below waiting for a message that had already gone by.
	id := c.send("stream.cancel", rpc.StreamCancel{ID: start.SubscriptionID})
	var (
		ok        rpc.OKResult
		end       rpc.StreamEnd
		acked     bool
		sawTheEnd bool
	)
	for !acked || !sawTheEnd {
		m := c.read()
		switch {
		case m.IsResponse() && string(m.ID) == id:
			if m.Error != nil {
				t.Fatalf("stream.cancel: %v", m.Error)
			}
			if err := json.Unmarshal(m.Result, &ok); err != nil {
				t.Fatalf("decoding the stream.cancel result: %v", err)
			}
			acked = true
		case m.IsNotification() && m.Method == rpc.MethodStreamEnd:
			if err := json.Unmarshal(m.Params, &end); err != nil {
				t.Fatalf("decoding stream.end: %v", err)
			}
			sawTheEnd = true
		}
	}
	if !ok.OK {
		t.Fatal("stream.cancel did not acknowledge")
	}

	select {
	case err := <-p.done:
		if err == nil {
			t.Fatal("producer stopped without a cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the stream did not stop the producer")
	}

	if end.Error != nil {
		t.Fatalf("a cancelled stream must end cleanly, got %v", end.Error)
	}

	// The id is gone: cancelling twice is a not_found, not a second cancel.
	if e := c.call("stream.cancel", rpc.StreamCancel{ID: start.SubscriptionID}, nil); e == nil {
		t.Fatal("cancelling an ended stream should be not_found")
	}
}

func TestStreamDisconnectCancelsTheProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newStreamProbe()
	RegisterHandler("test.streamDisconnect", p.handler(nil))
	t.Cleanup(func() { unregisterHandler("test.streamDisconnect") })

	h := newHarness(t, ctx)
	c := h.dial(ctx)

	var start rpc.StreamStart
	if e := c.call("test.streamDisconnect", rpc.Empty{}, &start); e != nil {
		t.Fatalf("starting stream: %v", e)
	}
	s := <-p.started

	c.conn.Close()

	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnecting did not stop the producer")
	}
	if s.Context().Err() == nil {
		t.Fatal("the stream context is still live after a disconnect")
	}
	if s.conn.CancelStream(s.ID()) {
		t.Fatal("the stream is still registered after a disconnect")
	}
}

func TestStreamSendFailsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, ctx)
	c := h.dial(ctx)
	h.srv.subsMu.RLock()
	var conn *Conn
	for _, cc := range h.srv.conns {
		conn = cc
	}
	h.srv.subsMu.RUnlock()
	_ = c

	sctx, scancel := context.WithCancel(ctx)
	s := &Stream{id: "sub-test", conn: conn, ctx: sctx, cancel: scancel}
	scancel()
	if err := s.Send(map[string]string{"line": "x"}); err == nil {
		t.Fatal("Send on a cancelled stream should fail")
	}
}

// TestSubscribeRepliesWithoutWaitingForAScan is contract §22's open follow-up:
// `state.subscribe {include: ["stats"]}` used to rescan before replying, so on
// a machine where collecting stats is slow the first snapshot arrived seconds
// late. The reply now comes from the cache — stats may be null in it — and the
// tick the subscription wakes delivers them in the first delta.
func TestSubscribeRepliesWithoutWaitingForAScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const scanDelay = 700 * time.Millisecond
	h := &testHarness{t: t}
	h.rows = []ports.ListeningPort{{
		Port: 8123, PID: 42, Process: "python3", Command: "python3 -m http.server",
	}}
	h.loop = scanner.New(scanner.Options{
		DaemonVersion: "test",
		Scan: func(inc scanner.Include) ([]ports.ListeningPort, error) {
			time.Sleep(scanDelay)
			h.mu.Lock()
			defer h.mu.Unlock()
			out := append([]ports.ListeningPort{}, h.rows...)
			if inc.Stats {
				for i := range out {
					out[i].CPUPercent, out[i].MemoryRSS = 12.5, 1<<20
				}
			}
			return out, nil
		},
	})
	h.srv = New(Options{Socket: "/test/sonar.sock", Version: "test", Scanner: h.loop})
	go h.loop.Run(ctx)
	c := h.dial(ctx)

	start := time.Now()
	var snap state.Snapshot
	if e := c.call("state.subscribe", rpc.StateSubscribeParams{
		Include: rpc.Include{"stats"},
	}, &snap); e != nil {
		t.Fatalf("state.subscribe: %v", e)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("state.subscribe replied after %v against a %v scan, want under 100ms", took, scanDelay)
	}

	// The woken tick carries the fields the subscriber asked for.
	delta := c.nextDelta()
	if len(delta.Ports.Added) != 1 {
		t.Fatalf("first delta = %+v, want the port", delta.Ports)
	}
	if delta.Ports.Added[0].Stats == nil || delta.Ports.Added[0].Stats.CPUPercent != 12.5 {
		t.Fatalf("first delta carries stats %+v, want the collected ones", delta.Ports.Added[0].Stats)
	}
	// Seq semantics (§15) are unchanged: the delta continues from the snapshot
	// the subscribe reply carried.
	if delta.Seq != snap.Seq+1 {
		t.Errorf("delta seq = %d, want the snapshot's %d plus one", delta.Seq, snap.Seq)
	}
}
