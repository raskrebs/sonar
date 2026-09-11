package groups

import (
	"errors"
	"strings"
	"testing"
)

func planConfig() *Config {
	return &Config{
		Path: "/tmp/demo/.sonar.yaml",
		Dir:  "/tmp/demo",
		Name: "demo",
		Services: []Service{
			{Name: "frontend", Cmd: "npm run dev", Port: 5173, DependsOn: []string{"api"}},
			{Name: "api", Cmd: "uv run api", Port: 8000, DependsOn: []string{"db", "cache"}},
			{Name: "db", Cmd: "postgres", Port: 5432},
			{Name: "cache", Cmd: "redis-server"},
		},
	}
}

func planNames(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Service.Name)
	}
	return out
}

// TestPlanOrdersDependenciesFirst: a service never starts before something it
// declares depends_on.
func TestPlanOrdersDependenciesFirst(t *testing.T) {
	steps, err := Plan(planConfig(), nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := planNames(steps)
	want := []string{"db", "cache", "api", "frontend"}
	if !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestPlanWaitsOnlyOnPortedDependencies: `cache` has no port, so there is
// nothing to wait for; `db` has one, so api waits for it.
func TestPlanWaitsOnlyOnPortedDependencies(t *testing.T) {
	steps, err := Plan(planConfig(), nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, s := range steps {
		switch s.Service.Name {
		case "api":
			if len(s.Waits) != 1 || s.Waits[0].Name != "db" {
				t.Errorf("api waits = %+v, want just db", s.Waits)
			}
		case "frontend":
			if len(s.Waits) != 1 || s.Waits[0].Name != "api" {
				t.Errorf("frontend waits = %+v, want just api", s.Waits)
			}
		default:
			if len(s.Waits) != 0 {
				t.Errorf("%s waits = %+v, want none", s.Service.Name, s.Waits)
			}
		}
	}
}

// TestPlanWaitsOnAutoPorts: a `port: auto` dependency has a port to wait for,
// even though the file names no number.
func TestPlanWaitsOnAutoPorts(t *testing.T) {
	cfg := &Config{Name: "demo", Services: []Service{
		{Name: "db", Cmd: "postgres", PortAuto: true},
		{Name: "api", Cmd: "uv run api", PortAuto: true, DependsOn: []string{"db"}},
	}}
	steps, err := Plan(cfg, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(steps) != 2 || len(steps[1].Waits) != 1 || steps[1].Waits[0].Name != "db" {
		t.Fatalf("steps = %+v, want api waiting on db", steps)
	}
}

// TestPlanOnlyFilters keeps the dependency order among what is left and does
// not drag unnamed services in.
func TestPlanOnlyFilters(t *testing.T) {
	steps, err := Plan(planConfig(), []string{"frontend", "db"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := planNames(steps); !equalStrings(got, []string{"db", "frontend"}) {
		t.Fatalf("order = %v, want [db frontend]", got)
	}
	// frontend still declares its wait: api may already be up, and
	// `groups.start` decides what to do about it.
	if len(steps[1].Waits) != 1 || steps[1].Waits[0].Name != "api" {
		t.Fatalf("frontend waits = %+v", steps[1].Waits)
	}
}

// TestPlanUnknownOnly reports a typo rather than starting nothing.
func TestPlanUnknownOnly(t *testing.T) {
	_, err := Plan(planConfig(), []string{"ap"})
	var unknown *UnknownServiceError
	if !errors.As(err, &unknown) || unknown.Name != "ap" {
		t.Fatalf("expected an UnknownServiceError for ap, got %v", err)
	}
	if !strings.Contains(err.Error(), "api") {
		t.Errorf("the error should list the real service names: %v", err)
	}
}

// TestPlanKeepsFileOrderAtTheSameDepth: two independent services start in the
// order the author wrote them.
func TestPlanKeepsFileOrderAtTheSameDepth(t *testing.T) {
	cfg := &Config{Name: "demo", Services: []Service{
		{Name: "one"}, {Name: "two"}, {Name: "three"},
	}}
	steps, err := Plan(cfg, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := planNames(steps); !equalStrings(got, []string{"one", "two", "three"}) {
		t.Fatalf("order = %v", got)
	}
}
