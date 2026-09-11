package groups

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	ports := map[string]int{"api": 21408, "db": 21407}
	for _, tt := range []struct{ in, want string }{
		{"--port ${port}", "--port 21408"},
		{"${url}/healthz", "http://localhost:21408/healthz"},
		{"postgres://localhost:${db.port}/app", "postgres://localhost:21407/app"},
		{"${db.url}", "http://localhost:21407"},
		// Not references: left exactly as written.
		{"${HOME}/bin", "${HOME}/bin"},
		{"${FOO:-x}", "${FOO:-x}"},
		{"$port", "$port"},
		// A reference to a service with no port in the map stays put.
		{"${cache.port}", "${cache.port}"},
	} {
		if got := Expand(tt.in, "api", ports); got != tt.want {
			t.Errorf("Expand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRefs(t *testing.T) {
	got := Refs("x ${port} ${db.port} ${HOME} ${db.url} ${url}", "api")
	if strings.Join(got, ",") != "api,db" {
		t.Errorf("Refs = %v, want [api db]", got)
	}
}

func TestMatchesExpanded(t *testing.T) {
	for _, tt := range []struct {
		pattern, actual string
		want            bool
	}{
		{"--port=${port}", "--port=8123", true},
		{"--port=${port}", "--port=", false},
		{"${port}", "8123", true},
		{"${HOME}", "${HOME}", true},
		{"${HOME}", "/Users/me", false},
		{"dev", "dev", true},
		{"dev", "build", false},
		{"a.b${port}", "axb1", false},
	} {
		if got := MatchesExpanded(tt.pattern, tt.actual); got != tt.want {
			t.Errorf("MatchesExpanded(%q, %q) = %v, want %v", tt.pattern, tt.actual, got, tt.want)
		}
	}
}

func TestLoadPortAutoAndEnv(t *testing.T) {
	cfg, err := loadString(t, "shop", `
name: shop
services:
  - name: db
    cmd: docker compose up db
    port: auto
  - name: api
    cmd: uv run uvicorn app:app --port ${port}
    port: auto
    depends_on: [db]
    env:
      DATABASE_URL: postgres://localhost:${db.port}/app
      HOME_BIN: ${HOME}/bin
  - name: worker
    cmd: uv run worker
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	api, _ := cfg.ServiceNamed("api")
	if !api.PortAuto || api.Port != 0 || !api.HasPort() {
		t.Errorf("api = %+v, want an auto port", api)
	}
	if api.Env["DATABASE_URL"] != "postgres://localhost:${db.port}/app" {
		t.Errorf("env = %v", api.Env)
	}
	if worker, _ := cfg.ServiceNamed("worker"); worker.HasPort() {
		t.Errorf("worker = %+v, want no port", worker)
	}

	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "port: auto") != 2 {
		t.Errorf("Marshal lost port: auto:\n%s", out)
	}
	back, err := Parse(cfg.Path, out)
	if err != nil {
		t.Fatalf("the marshalled file does not load: %v\n%s", err, out)
	}
	if db, _ := back.ServiceNamed("db"); !db.PortAuto {
		t.Errorf("round trip lost port: auto: %+v", db)
	}
}

func TestLoadReferenceProblems(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"a port that is neither", "services:\n  - name: api\n    port: fast\n",
			"port must be a number or auto"},
		{"an unknown service", "services:\n  - name: api\n    port: auto\n    cmd: run ${nope.port}\n",
			`names no service "nope"`},
		{"a service with no port", "services:\n  - name: db\n  - name: api\n    env:\n      DB: ${db.port}\n",
			"needs a port, and db declares none"},
		{"its own port when it has none", "services:\n  - name: api\n    cmd: run --port ${port}\n",
			"needs a port, and api declares none"},
		{"a bad variable name", "services:\n  - name: api\n    env:\n      BAD-KEY: x\n",
			`env "BAD-KEY" is not a variable name`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, "demo", "name: demo\n"+tt.body)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
