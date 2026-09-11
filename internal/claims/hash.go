// Package claims reserves ports for a (project, worktree) pair.
//
// A claim is book-keeping, not an OS-level bind: sonar records that a port is
// spoken for so parallel agents in different worktrees stop picking the same
// one, and so the same worktree gets the same ports every time. A process
// outside sonar can still take a claimed port, which is why every acquire
// probes the live scan before handing a port back.
//
// The derivation follows spec 2 §4: the key is `<project>/<worktree>`, the
// starting port is a stable hash of that key into a 10-port block of the
// configured range, and the probe walks upward from there.
package claims

import (
	"fmt"
	"strings"
)

// DefaultWorktree is the worktree name for a primary checkout, so a repo
// nobody has branched from still has a full key.
const DefaultWorktree = "main"

// BlockSize is the stride between derived starting ports: neighbouring keys
// land in different 10-port blocks, so a project needing a handful of ports
// takes them without stepping on the next key's block.
const BlockSize = 10

// Range is the window claims are derived from. DefaultRange follows spec 2 §4:
// 10-port blocks in 10000–32699, above the usual dev-server ports and below
// every OS ephemeral range (Linux 32768+, macOS 49152+), so a claimed port is
// never one the kernel hands out to an outgoing connection.
type Range struct {
	Start int
	End   int
}

// DefaultRange is the range used when a caller does not configure one.
var DefaultRange = Range{Start: 10000, End: 32699}

// Valid reports whether r is a usable port window.
func (r Range) Valid() error {
	if r.Start < 1 || r.End > 65535 || r.Start > r.End {
		return fmt.Errorf("claims: invalid port range %d-%d", r.Start, r.End)
	}
	return nil
}

// Span is how many ports the range holds.
func (r Range) Span() int { return r.End - r.Start + 1 }

// Key builds the claim key for a project and worktree. An empty worktree is
// the primary checkout, which the spec names "main".
func Key(project, worktree string) string {
	p := strings.Trim(strings.TrimSpace(project), "/")
	w := strings.Trim(strings.TrimSpace(worktree), "/")
	if w == "" {
		w = DefaultWorktree
	}
	return p + "/" + w
}

// ServiceKey is the claim key for one `port: auto` service in one checkout:
// the checkout's key plus the service name, so the service gets the same port
// every time that checkout starts it, and a different one in every other
// checkout of the project.
func ServiceKey(project, worktree, service string) string {
	return Key(project, worktree) + "/" + strings.TrimSpace(service)
}

// SplitKey recovers the project and worktree a key was built from. A key with
// no separator is all project, with the worktree defaulted.
func SplitKey(key string) (project, worktree string) {
	p, w, ok := strings.Cut(strings.TrimSpace(key), "/")
	if !ok || strings.TrimSpace(w) == "" {
		return p, DefaultWorktree
	}
	return p, w
}

// Base is the port a key's probe starts from: the same key always derives the
// same port, in this process and in every other one, because the hash is
// fnv-1a over the key alone.
func (r Range) Base(key string) int {
	h := fnv1a32(key)
	blocks := r.Span() / BlockSize
	if blocks < 1 {
		return r.Start + int(h%uint32(r.Span()))
	}
	return r.Start + int(h%uint32(blocks))*BlockSize
}

// At is the i-th candidate port for key, counting from Base and wrapping back
// to the start of the range rather than running off its end.
func (r Range) At(key string, i int) int {
	span := r.Span()
	offset := (r.Base(key) - r.Start + i%span + span) % span
	return r.Start + offset
}

// fnv1a32 is the 32-bit FNV-1a hash of s. Written out rather than taken from
// hash/fnv so the constants are visible: the derived port is part of the
// contract with every other sonar on the machine and must never drift.
func fnv1a32(s string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}
