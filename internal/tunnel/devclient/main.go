// Command devclient drives internal/tunnel by hand, for developing the share
// transport before the daemon has any of it.
//
// It is not part of the sonar binary and is not released: `sonar share` and
// the daemon's side of this come later, and this exists so the transport can
// be proved against a real relay and a real browser first.
//
//	SONAR_TUNNEL_KEY=… go run ./internal/tunnel/devclient \
//	    -relay http://127.0.0.1:8788 -port 5173
//
// A whole project under one hostname, the way `sonar share --project` lays one
// out, without needing an account or a deployed relay:
//
//	SONAR_TUNNEL_KEY=… go run ./internal/tunnel/devclient \
//	    -relay http://127.0.0.1:8788 -port 6873 \
//	    -at /_sonar/api=9700 -at /_sonar/admin=9800
//
// The key comes from the environment, never a flag: anyone who can run ps can
// read a command line.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/raskrebs/sonar/internal/tunnel"
)

func main() {
	relay := flag.String("relay", "http://127.0.0.1:8788",
		"The relay's base URL, or its /v1/tunnel route")
	port := flag.Int("port", 0, "The local port to share (required)")
	host := flag.String("local-host", "localhost", "The host the local app is on")
	debug := flag.Bool("v", false, "Log every forwarded request")
	grace := flag.Duration("grace", 0,
		"How long the shared port may stay dead before the share ends (0 = the default 90s)")
	watch := flag.Duration("watch", 0,
		"How often to check the shared port (0 = the default 2s)")
	var mounts mountList
	flag.Var(&mounts, "at",
		"Mount another service under a path: -at /_sonar/api=9700 (repeatable)")
	flag.Parse()

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "devclient: -port is required")
		os.Exit(2)
	}
	key := os.Getenv("SONAR_TUNNEL_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "devclient: set SONAR_TUNNEL_KEY to the relay's tunnel key")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := tunnel.Run(ctx, tunnel.Config{
		RelayURL:      *relay,
		Key:           key,
		LocalPort:     *port,
		LocalHost:     *host,
		Client:        "sonar-devclient",
		Logger:        log,
		ServiceGrace:  *grace,
		WatchInterval: *watch,
		Route:         mounts.route(*host, *port),
		OnStatus: func(s tunnel.Status) {
			switch s.State {
			case tunnel.StateConnected:
				fmt.Printf("share is live at %s -> %s:%d\n", s.URL, *host, *port)
			case tunnel.StateConnecting:
				if s.Err != nil {
					fmt.Printf("connecting (attempt %d): %v\n", s.Attempt+1, s.Err)
				} else {
					fmt.Println("connecting…")
				}
			case tunnel.StateDegraded:
				fmt.Println("degraded: the shared port is not answering")
			case tunnel.StateStopped:
				fmt.Println("stopped")
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "devclient: %v\n", err)
		os.Exit(1)
	}
}

// mountList is the -at flags: where each other service of a project sits.
//
// It is the same shape the daemon builds from a sonar.yaml — entry service at
// the root, everything else under a prefix that is stripped on the way in —
// written by hand so the transport can be driven against a real relay and a
// real browser before any of the daemon is involved.
type mountList []mount

type mount struct {
	prefix string
	addr   string
}

func (m *mountList) String() string { return fmt.Sprint(*m) }

func (m *mountList) Set(v string) error {
	prefix, port, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want <prefix>=<port>, got %q", v)
	}
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if !strings.HasPrefix(prefix, "/") || prefix == "" {
		return fmt.Errorf("%q is not a path", prefix)
	}
	if _, err := strconv.Atoi(strings.TrimSpace(port)); err != nil {
		return fmt.Errorf("%q is not a port", port)
	}
	*m = append(*m, mount{prefix: prefix, addr: net.JoinHostPort("localhost", strings.TrimSpace(port))})
	return nil
}

// route is the table as the tunnel asks for it. Longest prefix first, matching
// only on a segment boundary, and anything unclaimed falling to the entry
// service — the same rules internal/share applies, kept deliberately small
// here so this stays a tool rather than a second implementation.
func (m mountList) route(host string, port int) func(string) tunnel.Target {
	if len(m) == 0 {
		return nil
	}
	entry := net.JoinHostPort(host, strconv.Itoa(port))
	mounts := append(mountList(nil), m...)
	sort.SliceStable(mounts, func(i, j int) bool {
		return len(mounts[i].prefix) > len(mounts[j].prefix)
	})
	return func(path string) tunnel.Target {
		for _, mt := range mounts {
			if !strings.HasPrefix(path, mt.prefix) {
				continue
			}
			rest := path[len(mt.prefix):]
			if rest != "" && rest[0] != '/' {
				continue
			}
			if rest == "" {
				rest = "/"
			}
			return tunnel.Target{Addr: mt.addr, Path: rest, Prefix: mt.prefix}
		}
		return tunnel.Target{Addr: entry, Path: path}
	}
}
