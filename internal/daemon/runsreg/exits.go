package runsreg

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// How a run ended. A run sonar or the user asked to stop is stopped whatever
// its exit code; otherwise code 0 is exited and anything else crashed.
const (
	ReasonExited  = "exited"
	ReasonCrashed = "crashed"
	ReasonStopped = "stopped"
)

const (
	// maxExits caps the exit history.
	maxExits = 100
	// lastLineCount and tailBytes bound the log lines kept with an exit.
	lastLineCount = 20
	tailBytes     = 16 << 10
	// stopWindow is how long after an exit a kill may still correct it to
	// stopped: the kill can win the race against being marked as one.
	stopWindow = 30 * time.Second
)

// Meta is what a caller knows about where a run comes from, beyond the spawn
// request itself.
type Meta struct {
	ConfigPath string
	StartID    string
	Origin     string
}

// Origin is the client a request came from, as it named itself in
// daemon.hello: cli, app, mcp or tray.
func Origin(req *daemon.Request) string {
	if req == nil || req.Conn == nil {
		return ""
	}
	name, _, _ := req.Conn.Client()
	return name
}

// Exit is a run that ended: the record it had, and how it ended.
type Exit struct {
	Record
	Code      int
	Reason    string
	ExitedAt  time.Time
	LastLines []string
}

// ServiceExit is the exit as a service row carries it.
func (e Exit) ServiceExit() state.ServiceExit {
	return state.ServiceExit{
		Code:   e.Code,
		Reason: e.Reason,
		At:     e.ExitedAt.UTC().Format(time.RFC3339),
		RunID:  e.ID,
	}
}

func exitReason(code int, stopped bool) string {
	switch {
	case stopped:
		return ReasonStopped
	case code == 0:
		return ReasonExited
	default:
		return ReasonCrashed
	}
}

// Exited records that a run ended with code: it leaves the live runs and joins
// the exit history, with the last lines of its log. stopped says it was asked
// to stop — a Ctrl+C, `sonar kill`, `sonar down` — so a non-zero code is not a
// crash. It reports false for a pid that was not registered.
func (r *Registry) Exited(pid, code int, stopped bool) (Exit, bool) {
	rec, ok := r.Lookup(pid)
	if !ok {
		return Exit{}, false
	}
	var lines []string
	if rec.LogPath != "" {
		lines = tailLines(rec.LogPath, rec.LogOffset, lastLineCount)
	}
	now := r.clock()

	r.mu.Lock()
	rec, ok = r.runs[pid]
	if !ok {
		r.mu.Unlock()
		return Exit{}, false
	}
	delete(r.runs, pid)
	e := Exit{
		Record:    rec,
		Code:      code,
		Reason:    exitReason(code, stopped || rec.stopping),
		ExitedAt:  now,
		LastLines: lines,
	}
	r.exits = append(r.exits, e)
	if len(r.exits) > maxExits {
		r.exits = append([]Exit(nil), r.exits[len(r.exits)-maxExits:]...)
	}
	r.mu.Unlock()

	r.mirrorRemove(pid)
	return e, true
}

// Stopping marks runs sonar is about to stop, so their exit is recorded as
// stopped rather than a crash. A run that already exited has its record
// corrected instead, as long as it ended moments ago.
func (r *Registry) Stopping(pids []int) {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pid := range pids {
		if rec, ok := r.runs[pid]; ok {
			rec.stopping = true
			r.runs[pid] = rec
			continue
		}
		for i := len(r.exits) - 1; i >= 0; i-- {
			if e := &r.exits[i]; e.PID == pid && now.Sub(e.ExitedAt) < stopWindow {
				e.Reason = ReasonStopped
				break
			}
		}
	}
}

// LastExit implements groups.ExitHistory: how the latest run of a service
// ended, while nothing sonar started for that service is running.
func (r *Registry) LastExit(group, service string) (state.ServiceExit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.runs {
		if rec.Group == group && rec.Name == service {
			return state.ServiceExit{}, false
		}
	}
	for i := len(r.exits) - 1; i >= 0; i-- {
		if e := r.exits[i]; e.Group == group && e.Name == service {
			return e.ServiceExit(), true
		}
	}
	return state.ServiceExit{}, false
}

// Exits returns the exit history, newest first.
func (r *Registry) Exits() []Exit {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Exit, 0, len(r.exits))
	for i := len(r.exits) - 1; i >= 0; i-- {
		out = append(out, r.exits[i])
	}
	return out
}

// tailLines returns the last n lines a run wrote to its log: from offset, where
// the run began, or from the top of a file that was rotated since. At most
// tailBytes are read; a line cut by that limit is dropped.
func tailLines(path string, offset int64, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	if offset > size {
		offset = 0
	}
	start := offset
	if size-start > tailBytes {
		start = size - tailBytes
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	text := strings.TrimRight(string(buf), "\r\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if start > offset && len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

// serviceURL is the address a run's port hint stands for.
func serviceURL(port int) string { return "http://localhost:" + strconv.Itoa(port) }

// exitRow renders one exit for `runs.list`.
func exitRow(e Exit) rpc.RunRecord {
	code := e.Code
	out := rpc.RunRecord{
		ID:         e.ID,
		PID:        e.PID,
		Group:      e.Group,
		Name:       e.Name,
		Cmd:        e.Cmd,
		Cwd:        e.Cwd,
		StartedAt:  e.StartedAt.Format(time.RFC3339),
		Ports:      []int{},
		Status:     "exited",
		ConfigPath: e.ConfigPath,
		StartID:    e.StartID,
		Origin:     e.Origin,
		LogPath:    e.LogPath,
		ExitCode:   &code,
		Reason:     e.Reason,
		ExitedAt:   e.ExitedAt.UTC().Format(time.RFC3339),
		LastLines:  e.LastLines,
	}
	if e.PortHint > 0 {
		hint := e.PortHint
		out.PortHint = &hint
		out.URL = serviceURL(hint)
	}
	return out
}
