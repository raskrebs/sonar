package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/display"
)

// followPoll is how often a followed log is read again. A few reads a second
// cost nothing and behave the same on every OS, which a file watcher does not.
const followPoll = 200 * time.Millisecond

// followColors are the colours service prefixes take in turn.
var followColors = []func(string) string{display.Cyan, display.Magenta, display.Yellow, display.Green, display.Blue}

// followPrefix is what `sonar start` puts in front of a service's log lines:
// its name, padded to the widest name so the lines stay aligned, in a colour
// of its own.
func followPrefix(name string, width, i int) string {
	return followColors[i%len(followColors)](padRight(name, width)) + display.Dim(" | ")
}

// lineWriter writes whole lines from several followers to one writer, so two
// services never interleave inside a line.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) println(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.w, s)
}

// logFollower tails one service's log file from an offset and writes every
// line with the service's name in front, the way `docker compose up` does. The
// file is appended to across runs, so the offset is where this run began.
type logFollower struct {
	prefix string
	path   string
	offset int64
	out    *lineWriter

	partial []byte
	buf     []byte
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newLogFollower(out *lineWriter, prefix, path string, offset int64) *logFollower {
	return &logFollower{
		prefix: prefix, path: path, offset: offset, out: out,
		buf:  make([]byte, 32<<10),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start follows the file until Stop.
func (f *logFollower) Start() { go f.run() }

// Stop reads what is left, writes a last unterminated line if there is one,
// and returns once the follower has finished.
func (f *logFollower) Stop() {
	f.once.Do(func() { close(f.stop) })
	<-f.done
}

func (f *logFollower) run() {
	defer close(f.done)
	t := time.NewTicker(followPoll)
	defer t.Stop()
	for {
		f.poll()
		select {
		case <-f.stop:
			f.poll()
			if len(f.partial) > 0 {
				f.emit(string(f.partial))
				f.partial = nil
			}
			return
		case <-t.C:
		}
	}
}

// poll reads whatever was appended since the last read.
func (f *logFollower) poll() {
	file, err := os.Open(f.path)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return
	}
	if info.Size() < f.offset {
		// Rotated or truncated: the service is writing a new file from the top.
		f.offset = 0
		f.partial = nil
	}
	if info.Size() == f.offset {
		return
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return
	}
	for {
		n, err := file.Read(f.buf)
		if n > 0 {
			f.offset += int64(n)
			f.feed(f.buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// feed splits new bytes into lines, keeping an unterminated tail for later.
func (f *logFollower) feed(data []byte) {
	f.partial = append(f.partial, data...)
	for {
		i := bytes.IndexByte(f.partial, '\n')
		if i < 0 {
			break
		}
		f.emit(string(f.partial[:i]))
		f.partial = f.partial[i+1:]
	}
	f.partial = append([]byte(nil), f.partial...)
}

func (f *logFollower) emit(line string) {
	f.out.println(f.prefix + strings.TrimRight(line, "\r"))
}
