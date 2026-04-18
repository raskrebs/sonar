package ports

import (
	"path/filepath"
	"strings"
)

// resolveProcessName produces a human-readable name for a process from its
// command line, optionally augmented by its parent's command line and working
// directory. It is a pure function — all I/O happens upstream in Enrich.
//
// Resolution chain (each step may pass through to the next):
//  1. .app bundle detection (macOS desktop apps)
//  2. interpreter-aware basename (python script.py -> "script.py", uvicorn ...)
//  3. parent-process unwrap when the cmdline is a worker spawned by a known
//     supervisor (uvicorn --reload, gunicorn, multiprocessing.spawn)
//  4. cwd augmentation: prepend the project directory when it adds context
//     (e.g. "main" + cwd "acumence-api" -> "acumence-api/main")
func resolveProcessName(cmd, parentCmd, cwd string) string {
	if cmd == "" {
		return ""
	}

	name := interpreterAwareBasename(cmd)

	// If the resolved name is opaque (just an interpreter, or empty because
	// the cmdline was `python -c "..."`), try the parent process. Live-reload
	// supervisors and multiprocessing workers leave the meaningful invocation
	// in the parent.
	if isOpaqueName(name) && parentCmd != "" {
		if parentName := interpreterAwareBasename(parentCmd); !isOpaqueName(parentName) {
			name = parentName
		}
	}

	// Always augment with cwd context when it adds information.
	if cwd != "" {
		if augmented := augmentWithCwd(name, cwd); augmented != "" {
			return augmented
		}
	}
	return name
}

// interpreterAwareBasename extracts a meaningful name from a full command line.
// Handles .app bundles, scripting interpreters, and inline-code flags.
func interpreterAwareBasename(cmd string) string {
	// .app bundles: paths may contain spaces (e.g. "Application Support"),
	// so match before splitting on whitespace.
	if idx := strings.Index(cmd, ".app/"); idx >= 0 {
		return filepath.Base(cmd[:idx])
	}

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	exec := parts[0]
	base := filepath.Base(exec)

	// Inline-code flags: `python -c "from foo import ..."` / `node -e "..."`.
	// The next token is source code, not a filename — we can't extract a
	// useful name from it, so return just the interpreter and let the
	// caller fall back to the parent process.
	if len(parts) >= 2 && (parts[1] == "-c" || parts[1] == "-e") {
		return base
	}

	if isInterpreter(base) {
		// `python -m module.path arg`: -m takes a module name; show it.
		if name := extractModuleArg(parts); name != "" {
			return name
		}
		// `uv run uvicorn server:app`, `poetry run gunicorn ...`,
		// `npx next dev`: skip the runner verb and use the next token.
		if name := extractRunnerSubcommand(parts); name != "" {
			return name
		}
		// java -cp/-classpath: skip the classpath value, return main class.
		if base == "java" {
			if name := extractJavaMainClass(parts); name != "" {
				return name
			}
			return base
		}
		// `python /path/to/script.py arg1`: use the script basename.
		if name := extractScriptArg(parts); name != "" {
			return name
		}
		return base
	}

	// Generic binary: just use its basename. cwd augmentation (later) will
	// add project context if available.
	return base
}

// extractModuleArg handles `python -m foo.bar` -> "foo.bar".
func extractModuleArg(parts []string) string {
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "-m" {
			return parts[i+1]
		}
		if !strings.HasPrefix(parts[i], "-") {
			return ""
		}
	}
	return ""
}

// extractRunnerSubcommand handles `uv run uvicorn ...`, `poetry run gunicorn ...`,
// `npx next dev`, etc. Returns the subcommand basename (e.g. "uvicorn").
func extractRunnerSubcommand(parts []string) string {
	if len(parts) < 2 {
		return ""
	}
	exec := filepath.Base(parts[0])
	if exec != "uv" && exec != "poetry" && exec != "npx" && exec != "pipenv" && exec != "pdm" && exec != "rye" {
		return ""
	}
	// Find the first non-flag token after the runner verb.
	start := 1
	if exec == "uv" || exec == "poetry" || exec == "pipenv" || exec == "pdm" || exec == "rye" {
		// These take a verb like "run" before the subcommand.
		if len(parts) >= 3 && parts[1] == "run" {
			start = 2
		}
	}
	for i := start; i < len(parts); i++ {
		if !strings.HasPrefix(parts[i], "-") {
			return filepath.Base(parts[i])
		}
	}
	return ""
}

// extractScriptArg handles `python /path/script.py` -> "script.py".
func extractScriptArg(parts []string) string {
	for _, arg := range parts[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return filepath.Base(arg)
	}
	return ""
}

// extractJavaMainClass handles java command lines.
// Returns the main class for -cp/-classpath invocations,
// the jar basename for -jar, or "" if no main class found.
func extractJavaMainClass(parts []string) string {
	for i := 1; i < len(parts); i++ {
		arg := parts[i]
		if arg == "-jar" {
			if i+1 < len(parts) {
				return filepath.Base(parts[i+1])
			}
			return ""
		}
		if arg == "-cp" || arg == "-classpath" {
			i++ // skip the classpath value
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		// first non-flag, non-classpath-value token is the main class
		return arg
	}
	return ""
}

// augmentWithCwd prefixes the name with the cwd's basename when that adds
// useful project context. If the cwd basename is itself a noise directory
// (tmp, build, target/release, ...) it walks up the path to find the first
// meaningful ancestor.
func augmentWithCwd(name, cwd string) string {
	if name == "" || cwd == "" {
		return name
	}
	cwdBase := filepath.Base(cwd)
	if cwdBase == "" || cwdBase == "." || cwdBase == "/" {
		return name
	}
	// Walk past noise dirs to find a real project root.
	if isNoiseDir(cwdBase) {
		cwdBase = meaningfulAncestor(cwd)
		if cwdBase == "" {
			return name
		}
	}
	// Don't prepend if the cwd basename is already part of the name.
	if cwdBase == name || strings.HasPrefix(name, cwdBase+"/") {
		return name
	}
	// Don't prepend filesystem-root-ish names that don't identify a project.
	if cwdBase == "Users" || cwdBase == "home" {
		return name
	}
	return cwdBase + "/" + name
}

// meaningfulAncestor walks up from a path, skipping noise directory names,
// to find the first non-noise ancestor. Returns "" if none found.
func meaningfulAncestor(path string) string {
	for i := 0; i < 6; i++ {
		path = filepath.Dir(path)
		base := filepath.Base(path)
		if base == "" || base == "." || base == "/" {
			return ""
		}
		if !isNoiseDir(base) {
			return base
		}
	}
	return ""
}

// isInterpreter reports whether a basename is a scripting language runtime
// whose first arg is typically the meaningful target (a script or module).
func isInterpreter(base string) bool {
	switch base {
	case "python", "python2", "python3",
		"node", "nodejs", "deno", "bun",
		"ruby", "perl", "php",
		"java",
		"uv", "poetry", "pipenv", "pdm", "rye", "npx":
		return true
	}
	return false
}

// isOpaqueName reports whether a name is just an interpreter or runtime,
// giving the user no signal about what is actually running.
func isOpaqueName(name string) bool {
	switch name {
	case "", "python", "python2", "python3",
		"node", "nodejs", "deno", "bun",
		"ruby", "perl", "php", "java",
		"sh", "bash", "zsh", "fish",
		"uv", "poetry", "npx":
		return true
	}
	return false
}

// parseSystemdUnit extracts a systemd unit name from /proc/<pid>/cgroup contents.
// Handles both cgroup v1 and v2 layouts. Returns "" if no real unit is found.
func parseSystemdUnit(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		// cgroup v2 line: "0::/system.slice/nginx.service"
		// cgroup v1 line: "1:name=systemd:/system.slice/nginx.service"
		idx := strings.LastIndex(line, "/")
		if idx < 0 {
			continue
		}
		seg := line[idx+1:]
		if strings.HasSuffix(seg, ".service") || strings.HasSuffix(seg, ".scope") {
			// Skip user session scopes which aren't real services.
			if strings.HasPrefix(seg, "session-") || strings.HasPrefix(seg, "user@") {
				continue
			}
			return seg
		}
	}
	return ""
}

// isNoiseDir reports whether a directory name is a generic build / temp
// directory that doesn't identify a project (and so should be skipped when
// augmenting names with cwd context).
func isNoiseDir(name string) bool {
	switch name {
	case "tmp", "temp", "build", "dist", "bin", "out", "target",
		"release", "debug", "cmd", "src", "pkg",
		"node_modules", ".bin", ".venv", "venv", "env",
		"__pycache__", ".cache", ".tox":
		return true
	}
	return false
}
