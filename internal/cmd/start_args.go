package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/raskrebs/sonar/internal/groups"
)

// projectRequest is `sonar start` asked to start a project rather than one
// command: the sonar.yaml to start and, when named, which of its services.
type projectRequest struct {
	cfg      *groups.Config
	services []string
}

// classifyStart decides what `sonar start` was asked to do. dash is cobra's
// ArgsLenAtDash: -1 when there was no `--`.
//
//	sonar start                   every service in the nearest sonar.yaml
//	sonar start <dir> [service…]  the sonar.yaml in or above dir
//	sonar start <service…>        those services of the nearest sonar.yaml
//	sonar start -- <command>      one command
//
// It returns nil for a command. A command given without `--` still runs as
// one, as it always has: arguments that are not a directory, a config file or
// services of the nearest file are a command.
func classifyStart(cwd string, args []string, dash int) (*projectRequest, error) {
	switch {
	case dash == 0:
		return nil, nil
	case dash > 0:
		return nil, fmt.Errorf("`sonar start %s -- …`: name services, or give a command after --, not both",
			strings.Join(args[:dash], " "))
	}

	if len(args) == 0 {
		cfg, err := nearestConfig(cwd)
		if err != nil {
			return nil, err
		}
		return &projectRequest{cfg: cfg}, nil
	}

	if dir, ok := projectDirArg(cwd, args[0]); ok {
		cfg, err := nearestConfig(dir)
		if err != nil {
			return nil, err
		}
		for _, name := range args[1:] {
			if _, ok := cfg.ServiceNamed(name); !ok {
				return nil, unknownService(cfg, name)
			}
		}
		return &projectRequest{cfg: cfg, services: args[1:]}, nil
	}

	cfg, err := nearestConfig(cwd)
	if err != nil {
		return nil, nil
	}
	for _, name := range args {
		if _, ok := cfg.ServiceNamed(name); !ok {
			return nil, nil
		}
	}
	return &projectRequest{cfg: cfg, services: args}, nil
}

// projectDirArg reports whether an argument names a project rather than a
// command: a path to a directory, a directory holding a config, or a config
// file itself. It returns the directory to look for the config from.
func projectDirArg(cwd, arg string) (string, bool) {
	looksLikePath := arg == "." || arg == ".." || strings.ContainsAny(arg, `/\`) || strings.HasPrefix(arg, "~")
	path := arg
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "", false
	case info.IsDir():
		// A bare directory name counts only when it holds a config itself: a
		// command that happens to share its name with a folder here must still
		// run as a command.
		if looksLikePath || len(groups.FilesIn(path)) > 0 {
			return path, true
		}
		return "", false
	case groups.IsConfigName(filepath.Base(path)):
		return filepath.Dir(path), true
	}
	return "", false
}

// nearestConfig is the sonar.yaml at or above dir, confined to dir's own
// checkout when that is a linked worktree.
func nearestConfig(dir string) (*groups.Config, error) {
	index := groups.NewIndex()
	index.Observe(dir)
	if cfg := index.NearestFor(dir); cfg != nil {
		return cfg, nil
	}
	if bad := index.Invalid(); len(bad) > 0 {
		return nil, fmt.Errorf("%s cannot be used: %w", groups.ConfigName, bad[0].Err)
	}
	return nil, fmt.Errorf("no %s at or above %s\nhint: `sonar init` writes one, or start a single command: `sonar start -- <command>`",
		groups.ConfigName, shortPath(dir))
}

func unknownService(cfg *groups.Config, name string) error {
	known := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		known = append(known, s.Name)
	}
	return &groups.UnknownServiceError{Path: cfg.Path, Name: name, Known: known}
}
