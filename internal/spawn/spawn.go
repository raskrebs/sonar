// Package spawn starts a child process on behalf of `sonar start`, `sonar up`
// and the daemon's `runs.spawn`, in a way that makes the whole tree killable
// and attributable: the child gets its own process group (a Job Object on
// Windows), the run's identity in its environment, and — when detached — a
// rotated log file under ~/.config/sonar/logs.
//
// The package deliberately knows nothing about the daemon or the runs
// registry: callers register the returned Handle wherever it belongs.
package spawn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/raskrebs/sonar/internal/state"
)

// Environment variables every child gets, so a tool that wants to know which
// run it belongs to can read them (daemon spec, `sonar start` step 3).
const (
	EnvGroup    = "SONAR_GROUP"
	EnvName     = "SONAR_NAME"
	EnvRunID    = "SONAR_RUN_ID"
	EnvPortHint = "SONAR_PORT"
	// EnvSession and EnvSessionLabel re-export the agent session the caller
	// captured, in the `<tool>:<id>` form spec 2 §3 defines. A `sonar start`
	// nested inside this child therefore reads the session explicitly instead
	// of detecting it again, and every run of one agent shares one id.
	EnvSession      = "SONAR_SESSION"
	EnvSessionLabel = "SONAR_SESSION_LABEL"
)

// Request is one command to start.
type Request struct {
	// Argv is the command and its arguments, executed directly (never a shell).
	Argv []string
	// Cwd is the working directory; empty means the caller's own.
	Cwd string
	// Env is the base environment; nil means os.Environ(). The SONAR_* run
	// variables are appended to it either way.
	Env []string
	// Group and Name identify the run. Callers that have flags to honour
	// should run Resolve first; Spawn does not infer.
	Group string
	Name  string
	// PortHint is the port the command is expected to bind, or 0.
	PortHint int
	// Session is the agent session that asked for this run, captured by the
	// caller (internal/sessions). Spawn only carries it: it puts SONAR_SESSION
	// in the child's environment and hands it back on the Handle for the
	// registry to store. The zero value means no session.
	Session state.Session
	// Detach runs the child in its own session with stdout and stderr in the
	// run's log file, so it survives the process that started it.
	Detach bool
	// ID presets the run id; empty generates one.
	ID string
	// LogPath overrides the detached log file. Empty uses LogPath(group, name).
	LogPath string
	// Stdin, Stdout and Stderr are used in attached mode only; nil inherits
	// the caller's.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Handle is a started run: everything the registry needs, plus the process.
type Handle struct {
	ID        string
	PID       int
	PPID      int
	Group     string
	Name      string
	Cmd       string
	Cwd       string
	PortHint  int
	StartedAt time.Time
	LogPath   string
	// LogOffset is how long LogPath was when this run opened it: the file is
	// appended to across runs, so this run's output begins here.
	LogOffset int64
	Detached  bool
	Session   state.Session

	cmd *exec.Cmd
	log *os.File
}

// Spawn starts req's command. The returned Handle is live: attached callers
// Wait on it, detached callers register it and return.
func Spawn(ctx context.Context, req Request) (*Handle, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, errors.New("spawn: no command given")
	}
	cwd := req.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("spawn: resolving the working directory: %w", err)
		}
		cwd = wd
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("spawn: %s is not a directory", cwd)
	}

	id := req.ID
	if id == "" {
		id = NewID()
	}

	h := &Handle{
		ID:        id,
		PPID:      os.Getpid(),
		Group:     req.Group,
		Name:      req.Name,
		Cmd:       strings.Join(req.Argv, " "),
		Cwd:       cwd,
		PortHint:  req.PortHint,
		StartedAt: time.Now(),
		Detached:  req.Detach,
		Session:   req.Session,
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if ctx != nil {
		// The context bounds the start, not the child's life: a detached run
		// outlives every context its caller has.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	cmd.Dir = cwd
	cmd.Env = childEnv(req, id)

	if req.Detach {
		path := req.LogPath
		if path == "" {
			path = LogPath(req.Group, req.Name)
		}
		f, err := OpenLog(path)
		if err != nil {
			return nil, err
		}
		h.LogPath, h.log = path, f
		// After any rotation OpenLog did, so a fresh file starts at zero.
		if info, err := f.Stat(); err == nil {
			h.LogOffset = info.Size()
		}
		cmd.Stdin = nil
		cmd.Stdout = f
		cmd.Stderr = f
	} else {
		cmd.Stdin = req.Stdin
		cmd.Stdout = req.Stdout
		cmd.Stderr = req.Stderr
		if cmd.Stdin == nil {
			cmd.Stdin = os.Stdin
		}
		if cmd.Stdout == nil {
			cmd.Stdout = os.Stdout
		}
		if cmd.Stderr == nil {
			cmd.Stderr = os.Stderr
		}
	}

	// Platform-specific: own process group / session on Unix, Job Object plus
	// a new console process group on Windows.
	configure(cmd, req.Detach)

	if err := cmd.Start(); err != nil {
		if h.log != nil {
			h.log.Close()
		}
		return nil, fmt.Errorf("spawn: starting %q: %w", req.Argv[0], err)
	}
	if err := adopt(cmd, req.Detach); err != nil {
		_ = cmd.Process.Kill()
		if h.log != nil {
			h.log.Close()
		}
		return nil, fmt.Errorf("spawn: %w", err)
	}

	h.cmd = cmd
	h.PID = cmd.Process.Pid
	if h.log != nil && req.Detach {
		// The child owns the file descriptor now.
		h.log.Close()
		h.log = nil
	}
	return h, nil
}

// childEnv is the caller's environment plus the run's identity.
func childEnv(req Request, id string) []string {
	base := req.Env
	if base == nil {
		base = os.Environ()
	}
	drop := map[string]bool{EnvGroup: true, EnvName: true, EnvRunID: true, EnvPortHint: true}
	if req.Session.ID != "" {
		drop[EnvSession] = true
		drop[EnvSessionLabel] = true
	}
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok && drop[key] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, EnvGroup+"="+req.Group, EnvName+"="+req.Name, EnvRunID+"="+id)
	if req.PortHint > 0 {
		out = append(out, EnvPortHint+"="+strconv.Itoa(req.PortHint))
	}
	if req.Session.ID != "" {
		out = append(out, EnvSession+"="+req.Session.Tool+":"+req.Session.ID)
		if req.Session.Label != "" {
			out = append(out, EnvSessionLabel+"="+req.Session.Label)
		}
	}
	return out
}

// Wait blocks until the child exits and returns the exit code the caller should
// exit with: the child's own, or 128+signal when it was killed.
func (h *Handle) Wait() (int, error) {
	if h == nil || h.cmd == nil {
		return 0, errors.New("spawn: no process to wait for")
	}
	err := h.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		return signalExitCode(exitErr), nil
	}
	return 1, err
}

// Signal forwards sig to the child's whole process group, so a dev server's
// own children go down with it.
func (h *Handle) Signal(sig os.Signal) error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return errors.New("spawn: no process to signal")
	}
	return signalGroup(h.cmd.Process, sig)
}

// Kill terminates the child's process group (or Job Object) without waiting.
func (h *Handle) Kill() error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return errors.New("spawn: no process to kill")
	}
	return killGroup(h.cmd.Process)
}

// Process is the underlying child, for callers that need os.Process directly.
func (h *Handle) Process() *os.Process {
	if h == nil || h.cmd == nil {
		return nil
	}
	return h.cmd.Process
}

// NewID returns a short random run id, the same shape `sonar run --id` uses.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// Forwarder relays the interrupts this process receives to a child's process
// group (daemon spec, `sonar start` step 5).
//
// It must be created *before* the child is started. Installing the handler
// first is what guarantees the child starts with a default SIGINT disposition:
// exec turns a caught signal into the default action, while an ignored one
// stays ignored — and a `sonar start` line in a `dev.sh` is a background job,
// so it inherits an ignored SIGINT from the shell. Without this, Ctrl+C would
// reach neither sonar nor the dev server it is supervising.
type Forwarder struct {
	ch   chan os.Signal
	stop chan struct{}
	once sync.Once

	mu      sync.Mutex
	handle  *Handle
	pending []os.Signal
	// caught records that an interrupt arrived: the child was stopped by the
	// user rather than exiting on its own.
	caught bool
}

// Interrupted reports whether an interrupt reached this process while it
// supervised the child, so the child's exit was asked for.
func (f *Forwarder) Interrupted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caught
}

// CatchSignals starts catching SIGINT and SIGTERM. Nothing is forwarded until
// Forward names the child; signals that arrive before then are replayed to it,
// so a Ctrl+C in the moment between fork and registration is not lost.
func CatchSignals() *Forwarder {
	f := &Forwarder{ch: make(chan os.Signal, 8), stop: make(chan struct{})}
	signal.Notify(f.ch, os.Interrupt, syscall.SIGTERM)
	go f.run()
	return f
}

// Forward starts sending caught signals to h's process group.
func (f *Forwarder) Forward(h *Handle) {
	f.mu.Lock()
	f.handle = h
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	for _, sig := range pending {
		_ = h.Signal(sig)
	}
}

// Stop stops catching signals and restores the default behaviour.
func (f *Forwarder) Stop() {
	f.once.Do(func() {
		signal.Stop(f.ch)
		close(f.stop)
	})
}

func (f *Forwarder) run() {
	for {
		select {
		case sig := <-f.ch:
			f.mu.Lock()
			f.caught = true
			h := f.handle
			if h == nil {
				f.pending = append(f.pending, sig)
			}
			f.mu.Unlock()
			if h != nil {
				_ = h.Signal(sig)
			}
		case <-f.stop:
			return
		}
	}
}
