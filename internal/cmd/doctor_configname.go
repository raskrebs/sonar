package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/raskrebs/sonar/internal/groups"
	"github.com/spf13/cobra"
)

// fixLegacyConfigName renames this project's `.sonar.yaml` to sonar.yaml. It
// looks where `project_config` looks — the working directory, then the git
// root — and uses `git mv` when the file is tracked, so the rename is staged
// as one. It refuses when a file with the new name is already there: the two
// have to be reconciled by hand, and a rename would overwrite one of them.
func fixLegacyConfigName(context.Context, *cobra.Command) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dirs := []string{cwd}
	if root, _, ok := groups.Find(cwd); ok && root != cwd {
		dirs = append(dirs, root)
	}
	for _, dir := range dirs {
		present := groups.FilesIn(dir)
		if len(present) == 0 {
			continue
		}
		return renameLegacyConfig(dir, present)
	}
	return "", fmt.Errorf("no %s found at %s or its git root", groups.LegacyConfigName, cwd)
}

// renameLegacyConfig moves the file sonar reads in dir to ConfigName.
func renameLegacyConfig(dir string, present []string) (string, error) {
	from := present[0]
	if len(present) > 1 {
		return "", fmt.Errorf("%s has more than one config file; keep one by hand", dir)
	}
	if !groups.IsLegacyName(filepath.Base(from)) {
		return "", fmt.Errorf("%s already uses a current name", from)
	}
	to := filepath.Join(dir, groups.ConfigName)

	if tracked(dir, filepath.Base(from)) {
		out, err := exec.Command("git", "-C", dir, "mv", filepath.Base(from), groups.ConfigName).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git mv: %v: %s", err, out)
		}
		return fmt.Sprintf("ran `git mv %s %s` in %s", filepath.Base(from), groups.ConfigName, dir), nil
	}
	// Lstat before Rename: on Unix a rename replaces an existing file silently.
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	if err := os.Rename(from, to); err != nil {
		return "", err
	}
	return fmt.Sprintf("renamed %s to %s", from, to), nil
}

// tracked reports whether git tracks name in dir. No git, or no repository,
// is simply "not tracked".
func tracked(dir, name string) bool {
	return exec.Command("git", "-C", dir, "ls-files", "--error-unmatch", "--", name).Run() == nil
}
