package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/raskrebs/sonar/internal/config"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/groups"
	"gopkg.in/yaml.v3"
)

// BrokenSuffix is how `sonar doctor --fix` names the file it moves aside:
// config.yaml.broken-20260906T121314Z. Nothing is ever deleted.
const BrokenSuffix = ".broken-"

// checkConfigParses reads the user's own config.yaml. A file that does not
// parse is the one failure here that silently changes what every other command
// does — config.Load drops the whole file on a syntax error and carries on with
// defaults — so it fails loudly and points at the exact bracket.
func checkConfigParses(_ context.Context, env *Env) rpc.DoctorCheck {
	path := env.ConfigPath
	if path == "" {
		return rpc.DoctorCheck{Status: StatusSkip, Summary: "no config path resolved"}
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return rpc.DoctorCheck{
			Status:  StatusOK,
			Summary: "no config file, defaults apply",
			Detail:  path + " does not exist, which is a valid state",
			Fix:     "sonar config init writes a commented starter file",
		}
	}
	if err != nil {
		return rpc.DoctorCheck{
			Status:  StatusFail,
			Summary: "cannot read the config file",
			Detail:  err.Error(),
		}
	}

	src := string(data)
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		pos := parsePosition(src, err)
		where := path
		if pos.Line > 0 {
			where = fmt.Sprintf("%s:%d:%d", path, pos.Line, pos.Col)
		}
		detail := fmt.Sprintf("%s: %s", where, yamlMessage(err))
		if marked := caret(src, pos); marked != "" {
			detail += "\n" + marked
		}
		return rpc.DoctorCheck{
			Status:  StatusFail,
			Summary: "config.yaml does not parse",
			Detail:  detail,
			Fix: fmt.Sprintf("fix %s, or run `sonar doctor --fix` to move it to %s%s<timestamp> and write a fresh template",
				path, filepath.Base(path), BrokenSuffix),
			Fixable: true,
		}
	}

	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: "config.yaml parses but some settings have the wrong type",
			Detail:  fmt.Sprintf("%s: %v", path, err),
			Fix:     "sonar config edit",
		}
	}
	return rpc.DoctorCheck{
		Status:  StatusOK,
		Summary: "config.yaml parses",
		Detail:  path,
	}
}

func checkConfigDirWritable(_ context.Context, env *Env) rpc.DoctorCheck {
	dir := env.ConfigDir
	if dir == "" {
		return rpc.DoctorCheck{Status: StatusSkip, Summary: "no config directory resolved"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return rpc.DoctorCheck{
			Status:  StatusFail,
			Summary: "cannot create the config directory",
			Detail:  fmt.Sprintf("%s: %v", dir, err),
			Fix:     "create it yourself, or point HOME somewhere writable",
		}
	}
	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return rpc.DoctorCheck{
			Status:  StatusFail,
			Summary: "config directory is not writable",
			Detail:  fmt.Sprintf("%s: %v", dir, err),
			Fix:     fmt.Sprintf("chmod u+w %s (the daemon writes its log, lock and database here)", dir),
		}
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return rpc.DoctorCheck{
		Status:  StatusOK,
		Summary: "config directory is writable",
		Detail:  dir,
	}
}

// checkProjectConfig looks for this project's `sonar.yaml`, in the directory
// the caller named and then at the git root above it — the two places every
// other command looks.
func checkProjectConfig(_ context.Context, env *Env) rpc.DoctorCheck {
	dir := env.Project
	if dir == "" {
		return rpc.DoctorCheck{Status: StatusSkip, Summary: "no project directory"}
	}
	root := gitRootOf(dir)

	var looked []string
	for _, base := range []string{dir, root} {
		if base == "" {
			continue
		}
		if present := groups.FilesIn(base); len(present) > 0 {
			return projectConfigResult(present[0], present[1:])
		}
		for _, name := range groups.ConfigNames() {
			looked = append(looked, filepath.Join(base, name))
		}
		if root == dir {
			break
		}
	}

	detail := "looked at " + strings.Join(dedupe(looked), ", ")
	if root == "" {
		detail += fmt.Sprintf("\n%s is not inside a git repository, so there is no repository root to look at either", dir)
	}
	return rpc.DoctorCheck{
		Status:  StatusWarn,
		Summary: "no " + groups.ConfigName + " for this project",
		Detail:  detail,
		Fix:     "sonar init writes one from what is listening right now",
	}
}

// projectConfigResult reports the config sonar reads for a project. shadowed
// are the other spellings sitting next to it, which sonar ignores; a file under
// the old dotfile name still works, and the check says how to rename it.
func projectConfigResult(path string, shadowed []string) rpc.DoctorCheck {
	cfg, err := groups.Load(path)
	if err != nil {
		return rpc.DoctorCheck{
			Status:  StatusFail,
			Summary: filepath.Base(path) + " does not load",
			Detail:  err.Error(),
			Fix:     "edit " + path,
		}
	}
	summary := fmt.Sprintf("group %q, %d %s", cfg.Name, len(cfg.Services),
		plural(len(cfg.Services), "service"))
	base := filepath.Base(path)

	if len(shadowed) > 0 {
		names := make([]string, 0, len(shadowed))
		for _, s := range shadowed {
			names = append(names, filepath.Base(s))
		}
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: fmt.Sprintf("%s, but %s is ignored next to %s", summary, strings.Join(names, " and "), base),
			Detail:  fmt.Sprintf("sonar reads %s and ignores %s", path, strings.Join(shadowed, ", ")),
			Fix:     "move anything you still need into " + base + ", then delete " + strings.Join(names, " and "),
		}
	}
	if groups.IsLegacyName(base) {
		return rpc.DoctorCheck{
			Status:  StatusWarn,
			Summary: fmt.Sprintf("%s, under the old name %s", summary, base),
			Detail:  path + " still works; new projects get " + groups.ConfigName,
			Fix:     "git mv " + base + " " + groups.ConfigName,
			Fixable: true,
		}
	}
	return rpc.DoctorCheck{
		Status:  StatusOK,
		Summary: summary,
		Detail:  path,
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
