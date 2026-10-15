// Package remoteinstall installs sonar on a remote host over SSH.
//
// It drives the system `ssh` binary — never a Go SSH library — so that
// `~/.ssh/config`, jump hosts, agents and every other thing the user has
// already configured keep working unchanged (spec, "Transport").
//
// The release archive is downloaded *on the remote host* and verified there
// against the sha256 the release publishes (spec, decision 2): the desktop app
// bundles no Linux binaries, and the bytes never make a round trip through the
// machine running the CLI.
package remoteinstall

import (
	"fmt"
	"strings"
)

// Platform is a remote host's OS and architecture, in Go's spelling, as
// `uname -s -m` reported them.
type Platform struct {
	OS   string // linux | darwin
	Arch string // amd64 | arm64
	// Kernel and Machine are the raw `uname` words, kept for error messages:
	// "aarch64" is what the user sees on the box, not "arm64".
	Kernel  string
	Machine string
}

// String is the "linux/amd64" form the CLI and the chunks print.
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Asset is the release asset that carries this platform's binary. The names
// come from .github/workflows/release.yml: sonar_<os>_<arch>.tar.gz everywhere
// but Windows, which ships a .zip and is out of scope here.
func (p Platform) Asset() string {
	return fmt.Sprintf("sonar_%s_%s.tar.gz", p.OS, p.Arch)
}

// AssetURL is where a release publishes that asset.
func (p Platform) AssetURL(version string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repo, version, p.Asset())
}

// Repo is the GitHub repository the releases come from.
const Repo = "raskrebs/sonar"

// UnsupportedPlatformError is a remote host sonar cannot install on. It is its
// own type so the daemon handler can turn it into an `unsupported` rpc error
// rather than an internal one.
type UnsupportedPlatformError struct {
	Kernel  string
	Machine string
	Reason  string
}

func (e *UnsupportedPlatformError) Error() string {
	return fmt.Sprintf("cannot install sonar on %s %s: %s", e.Kernel, e.Machine, e.Reason)
}

// ParsePlatform maps the output of `uname -s -m` onto a release asset.
//
// Linux amd64 and arm64 are the supported targets; macOS is accepted because
// the release publishes darwin archives anyway. Windows hosts are out of scope
// for milestone 3 and get a message that says so rather than a 404 from a
// download of an asset that does not exist for them.
func ParsePlatform(uname string) (Platform, error) {
	fields := strings.Fields(strings.TrimSpace(uname))
	if len(fields) < 2 {
		return Platform{}, fmt.Errorf("could not read the remote platform: `uname -s -m` printed %q", strings.TrimSpace(uname))
	}
	kernel, machine := fields[0], fields[len(fields)-1]
	p := Platform{Kernel: kernel, Machine: machine}

	switch {
	case strings.EqualFold(kernel, "linux"):
		p.OS = "linux"
	case strings.EqualFold(kernel, "darwin"):
		p.OS = "darwin"
	case hasAnyPrefixFold(kernel, "mingw", "msys", "cygwin", "windows"):
		return Platform{}, &UnsupportedPlatformError{Kernel: kernel, Machine: machine,
			Reason: "Windows hosts are not supported yet; install sonar there with the PowerShell installer, then `sonar remote add` it with `--remote-bin sonar`"}
	default:
		return Platform{}, &UnsupportedPlatformError{Kernel: kernel, Machine: machine,
			Reason: "only Linux and macOS hosts are supported"}
	}

	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64", "armv8l":
		p.Arch = "arm64"
	case "i386", "i486", "i586", "i686", "x86":
		return Platform{}, &UnsupportedPlatformError{Kernel: kernel, Machine: machine,
			Reason: "sonar publishes no 32-bit x86 build"}
	default:
		return Platform{}, &UnsupportedPlatformError{Kernel: kernel, Machine: machine,
			Reason: "sonar publishes builds for x86_64 and aarch64 only"}
	}
	return p, nil
}

func hasAnyPrefixFold(s string, prefixes ...string) bool {
	lower := strings.ToLower(s)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
