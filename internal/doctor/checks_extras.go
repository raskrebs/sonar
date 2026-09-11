package doctor

import (
	"context"
	"errors"
	"fmt"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/tray"
)

// checkDocker is about enrichment, never about health: sonar works fine with no
// docker on the machine. The one thing worth reporting is the confusing case —
// the CLI is installed but its daemon is not answering, so container names and
// images are silently missing from every listing.
func checkDocker(ctx context.Context, env *Env) rpc.DoctorCheck {
	path, err := env.LookPath("docker")
	if err != nil {
		return rpc.DoctorCheck{
			Status:  StatusSkip,
			Summary: "docker is not installed",
			Detail:  "container names, images and compose projects are not enriched",
		}
	}
	version, err := env.Docker(ctx)
	if errors.Is(err, ErrDockerNotAnswering) {
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: "docker CLI not answering",
			Detail:  fmt.Sprintf("%s: %v", path, err),
			Fix: "Docker is running but wedged: restart Docker Desktop (or the docker service); " +
				"until then the daemon keeps the last container list it saw and retries with backoff",
		}
	}
	if err != nil {
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: "docker is installed but not responding",
			Detail:  fmt.Sprintf("%s: %v", path, err),
			Fix:     "start Docker Desktop (or the docker service); until then container rows are unenriched",
		}
	}
	return rpc.DoctorCheck{
		Status:  StatusOK,
		Summary: "docker is responding",
		Detail:  fmt.Sprintf("%s, server %s", path, version),
	}
}

// checkTray looks for the legacy macOS menu bar binary. It is not broken, but
// it is superseded: `sonar tray` prefers the desktop app wherever both are
// installed, so a leftover sonar-tray is a thing to remove, not to launch.
func checkTray(_ context.Context, env *Env) rpc.DoctorCheck {
	if env.GOOS != "darwin" {
		return rpc.DoctorCheck{
			Status:  StatusSkip,
			Summary: "macOS only",
			Detail:  "the legacy Swift menu bar binary only ever shipped for macOS",
		}
	}
	path, ok := tray.LegacyTray()
	if !ok {
		return rpc.DoctorCheck{
			Status:  StatusOK,
			Summary: "no legacy menu bar binary",
			Detail:  "`sonar tray` launches the desktop app",
		}
	}
	return rpc.DoctorCheck{
		Status:  StatusWarn,
		Summary: "the legacy sonar-tray is still installed",
		Detail:  path + " predates the desktop app, which replaces it",
		Fix:     "rm " + path + " — `sonar tray` then launches the desktop app",
	}
}
