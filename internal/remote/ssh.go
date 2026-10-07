package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Dialer opens one bridge to a host. It returns the stream the daemon protocol
// is spoken over: the far side's `sonar daemon stdio`. Tests replace it with a
// pipe to an in-process daemon so the whole manager can be exercised with no
// subprocess, no SSH and no network.
type Dialer func(ctx context.Context, h Host) (io.ReadWriteCloser, error)

// RemoteCommand is what the bridge asks the far side to run.
const RemoteCommand = "daemon stdio"

// DefaultRemoteBinary is the binary the far side runs when the host config
// does not name one.
const DefaultRemoteBinary = "sonar"

// defaultBinDir is where `sonar remote install` puts the binary. It is added
// to the far side's PATH for the default binary, because the shell ssh runs a
// command in is neither a login shell nor an interactive one: on a stock
// Debian or Ubuntu it reads no file that would put ~/.local/bin on PATH, so a
// bare `sonar` is "command not found" on a host the install just set up.
const defaultBinDir = "$HOME/.local/bin"

// FarCommand is the command line ssh is asked to run on the far side: the
// host's `sonar daemon stdio`, plus any extra flags. The bridge and the
// install's final check both build it here, so a check that passes proves the
// bridge will connect.
//
// With no binary named it looks in ~/.local/bin first and then on the remote
// PATH. That goes through `sh -c` because the user's login shell runs whatever
// ssh is given, and it need not be a POSIX one. A named binary is run as
// written.
func FarCommand(bin string, extra ...string) string {
	command := RemoteCommand
	if len(extra) > 0 {
		command += " " + strings.Join(extra, " ")
	}
	if bin = strings.TrimSpace(bin); bin != "" {
		return bin + " " + command
	}
	return `sh -c 'PATH="` + defaultBinDir + `:$PATH"; exec ` + DefaultRemoteBinary + " " + command + `'`
}

// SSHArgs builds the argument list for one host, in the order `ssh` wants it:
// options first, then the target, then the remote command. The user's own
// ssh_args come last among the options so they can override an identity or a
// port sonar would otherwise pass.
func SSHArgs(h Host) []string {
	args := make([]string, 0, 8+len(h.SSHArgs))
	// Batch mode: a bridge runs unattended, so a password or passphrase prompt
	// would block forever on a stdin that is carrying JSON-RPC. Failing fast
	// turns that into a reconnect with a readable reason instead.
	args = append(args, "-o", "BatchMode=yes")
	if h.Port > 0 {
		args = append(args, "-p", strconv.Itoa(h.Port))
	}
	if h.Identity != "" {
		args = append(args, "-i", expandHome(h.Identity))
	}
	args = append(args, h.SSHArgs...)
	args = append(args, h.Target, FarCommand(h.RemoteBin))
	return args
}

// expandHome turns a leading ~ into the user's home directory. `ssh -i` does
// not do it for us: the shell normally would, and there is no shell here.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + path[1:]
		}
	}
	return path
}

// SSHBinary is the ssh executable the bridge spawns. It is a variable so a
// test can point it at a stand-in on PATH.
var SSHBinary = "ssh"

// DialSSH is the production Dialer: it spawns `ssh <opts> <target> sonar
// daemon stdio` and hands back its pipes. Closing the stream kills the ssh
// process, which is what tears the bridge down on remote.remove.
func DialSSH(ctx context.Context, h Host) (io.ReadWriteCloser, error) {
	if _, err := exec.LookPath(SSHBinary); err != nil {
		return nil, fmt.Errorf("cannot find the %s binary: %w", SSHBinary, err)
	}
	cmd := exec.Command(SSHBinary, SSHArgs(h)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	// ssh writes its own diagnostics to stderr — "Permission denied", "Host
	// key verification failed", "sonar: command not found" from the remote
	// shell. Keeping the tail of it is what makes a host's status_reason say
	// something a user can act on instead of "EOF".
	var errBuf tailBuffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("starting %s: %w", SSHBinary, err)
	}
	return &process{cmd: cmd, in: stdin, out: stdout, errs: &errBuf}, nil
}

// process is a spawned `ssh ... sonar daemon stdio` seen as one stream.
type process struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	errs *tailBuffer

	once sync.Once
}

func (p *process) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if errors.Is(err, io.EOF) {
		// EOF on the far side's stdout is the whole story only when ssh had
		// nothing to say. When it did, that is the diagnosis.
		if msg := p.errs.String(); msg != "" {
			return n, fmt.Errorf("%s: %s", io.EOF, msg)
		}
	}
	return n, err
}

func (p *process) Write(b []byte) (int, error) { return p.in.Write(b) }

// Close tears the bridge down: the far side sees EOF on its stdin, and the ssh
// process is killed if it does not take the hint.
func (p *process) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		_ = p.out.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
	})
	return nil
}

// tailBuffer keeps the last few hundred bytes written to it. ssh in verbose
// mode can write a lot; a status_reason only needs the end of it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferMax = 512

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > tailBufferMax {
		t.buf = t.buf[len(t.buf)-tailBufferMax:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.ReplaceAll(string(t.buf), "\n", "; "))
}
