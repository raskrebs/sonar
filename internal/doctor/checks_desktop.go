package doctor

import (
	"context"
	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/desktop"
	"github.com/raskrebs/sonar/internal/tray"
)

// checkDesktopInstalled answers the question `sonar tray` asks at launch: is
// the app on this machine, and which one.
//
// It reads the config first, because that is the only source that knows the
// *version* — the record `sonar install desktop` leaves behind — and falls
// back to the same lookup tray does, so an app installed by some other means
// (a .deb, a drag into /Applications) is still reported as installed rather
// than as missing. A recorded path that no longer exists is the interesting
// third case: the config says one thing and the disk says another, and the
// same one command fixes it.
func checkDesktopInstalled(_ context.Context, env *Env) rpc.DoctorCheck {
	if !desktop.Supported(env.GOOS) {
		return rpc.DoctorCheck{
			Status:  StatusSkip,
			Summary: "no desktop app for " + env.GOOS + " yet",
			Detail:  "`sonar install desktop` publishes macOS and Linux builds so far",
		}
	}

	cfg, _ := config.LoadFrom(env.ConfigPath)
	recorded := cfg.Desktop
	if recorded.InstalledPath != "" && env.exists(recorded.InstalledPath) {
		version := recorded.InstalledVersion
		if version == "" {
			version = "an unrecorded version"
		}
		return rpc.DoctorCheck{
			Status:  StatusOK,
			Summary: "Sonar " + version + " is installed",
			Detail:  recorded.InstalledPath,
		}
	}

	if path, ok := installedElsewhere(env); ok {
		return rpc.DoctorCheck{
			Status:  StatusOK,
			Summary: "the desktop app is installed",
			Detail:  path + " — sonar did not install it, so there is no version on record",
		}
	}

	if recorded.InstalledPath != "" {
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: "the recorded desktop app is gone",
			Detail:  recorded.InstalledPath + " no longer exists",
			Fix:     "sonar install desktop",
		}
	}
	return rpc.DoctorCheck{
		Status:  StatusWarn,
		Summary: "the desktop app is not installed",
		Detail:  "`sonar tray` has nothing to launch",
		Fix:     "sonar install desktop",
	}
}

// installedElsewhere runs tray's own lookup against the doctor's seams, so the
// two never disagree about what counts as installed.
func installedElsewhere(env *Env) (string, bool) {
	d := tray.Decide(tray.Env{
		GOOS:     env.GOOS,
		Home:     env.Home,
		Getenv:   func(string) string { return "" },
		Exists:   env.exists,
		LookPath: env.LookPath,
	})
	if d.Kind == tray.KindDesktopApp {
		return d.Path, true
	}
	return "", false
}
