package ports

import "testing"

func TestResolveProcessName(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		parentCmd string
		cwd       string
		want      string
	}{
		// .app bundles
		{
			name: ".app bundle",
			cmd:  "/Applications/Visual Studio Code.app/Contents/MacOS/Electron --type=renderer",
			want: "Visual Studio Code",
		},

		// Generic Go binary with cwd context
		{
			name: "Go binary in tmp/main with cwd",
			cmd:  "/Users/me/projects/acumence-api/tmp/main",
			cwd:  "/Users/me/projects/acumence-api",
			want: "acumence-api/main",
		},
		{
			name: "Go binary without cwd falls back to basename",
			cmd:  "/Users/me/projects/acumence-api/tmp/main",
			want: "main",
		},
		{
			name: "binary in noise dir uses ancestor",
			cmd:  "/Users/me/projects/myapp/build/server",
			cwd:  "/Users/me/projects/myapp/build",
			want: "myapp/server",
		},

		// Python -c (multiprocessing.spawn worker)
		{
			name: "python -c with parent uvicorn",
			cmd:  "/usr/bin/python -c from multiprocessing.spawn import spawn_main; spawn_main(...)",
			parentCmd: "/Users/me/.venv/bin/python /Users/me/.venv/bin/uvicorn server:app --reload --port 8001",
			want: "uvicorn",
		},
		{
			name:      "python -c with no parent falls back to interpreter",
			cmd:       "python -c from foo import bar",
			parentCmd: "",
			want:      "python",
		},

		// uv / poetry runners
		{
			name: "uv run uvicorn",
			cmd:  "uv run uvicorn server:app --port 8001",
			want: "uvicorn",
		},
		{
			name: "poetry run gunicorn",
			cmd:  "poetry run gunicorn app:application",
			want: "gunicorn",
		},
		{
			name: "npx next dev",
			cmd:  "npx next dev",
			want: "next",
		},

		// python script vs module
		{
			name: "python /path/script.py",
			cmd:  "/usr/bin/python /home/me/app/server.py --port 8000",
			want: "server.py",
		},
		{
			name: "python -m module",
			cmd:  "python -m uvicorn server:app",
			want: "uvicorn",
		},
		{
			name: "python -m dotted.module",
			cmd:  "python -m my.app.main",
			want: "my.app.main",
		},

		// node
		{
			name: "node script.js",
			cmd:  "/usr/local/bin/node /app/server.js",
			want: "server.js",
		},

		// java
		{
			name: "java -cp classpath ClassName",
			cmd:  "/usr/bin/java -cp dep1.jar:dep2.jar:dep3.jar com.myapp.SomeClass",
			want: "com.myapp.SomeClass",
		},
		{
			name: "java -classpath classpath ClassName",
			cmd:  "java -classpath /path/to/dep1.jar:/path/to/dep2.jar com.example.Main",
			want: "com.example.Main",
		},
		{
			name: "java -jar app.jar",
			cmd:  "java -jar /path/to/app.jar",
			want: "app.jar",
		},
		{
			name: "java with no class",
			cmd:  "java",
			want: "java",
		},

		// Cwd should not be added when name already starts with it
		{
			name: "no double-prefix when name already includes cwd",
			cmd:  "/usr/bin/myapp",
			cwd:  "/projects/myapp",
			want: "myapp",
		},
		// Cwd ignored for system home root
		{
			name: "skip Users home root",
			cmd:  "/usr/local/bin/redis-server",
			cwd:  "/Users",
			want: "redis-server",
		},

		// Cwd of generic binary in build dir walks up
		{
			name: "cargo target/release walks up",
			cmd:  "/projects/myapp/target/release/myapp-server",
			cwd:  "/projects/myapp/target/release",
			want: "myapp/myapp-server",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveProcessName(tc.cmd, tc.parentCmd, tc.cwd)
			if got != tc.want {
				t.Errorf("\ncmd:       %q\nparentCmd: %q\ncwd:       %q\ngot:  %q\nwant: %q",
					tc.cmd, tc.parentCmd, tc.cwd, got, tc.want)
			}
		})
	}
}

func TestParseSystemdUnit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "cgroup v2 system service",
			in:   "0::/system.slice/nginx.service\n",
			want: "nginx.service",
		},
		{
			name: "cgroup v2 user app",
			in:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/myapp.service\n",
			want: "myapp.service",
		},
		{
			name: "cgroup v1",
			in:   "12:freezer:/\n11:devices:/system.slice/postgresql.service\n0::/\n",
			want: "postgresql.service",
		},
		{
			name: "scope unit",
			in:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-something.scope\n",
			want: "app-something.scope",
		},
		{
			name: "no unit",
			in:   "0::/user.slice/user-1000.slice/session-2.scope\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSystemdUnit(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
