package cmd

import (
	"reflect"
	"testing"

	"github.com/raskrebs/sonar/internal/display"
)

func TestEffectiveString(t *testing.T) {
	// Flag explicitly set wins over config.
	if got := effectiveString(true, "pid", "name"); got != "pid" {
		t.Errorf("changed flag should win, got %q", got)
	}
	// Flag unchanged, config provides value.
	if got := effectiveString(false, "port", "name"); got != "name" {
		t.Errorf("config should win when flag unchanged, got %q", got)
	}
	// Flag unchanged, no config value -> keep flag default.
	if got := effectiveString(false, "port", ""); got != "port" {
		t.Errorf("default should win when no config, got %q", got)
	}
}

func TestEffectiveBool(t *testing.T) {
	tru := true
	fls := false
	if got := effectiveBool(true, true, &fls); got != true {
		t.Error("changed flag should win over config")
	}
	if got := effectiveBool(false, false, &tru); got != true {
		t.Error("config should win when flag unchanged")
	}
	if got := effectiveBool(false, false, nil); got != false {
		t.Error("default should win when config nil")
	}
}

func TestEffectiveColumns(t *testing.T) {
	// --all-columns wins.
	if got := effectiveColumns(true, "", false, []string{"port"}); !reflect.DeepEqual(got, display.AllColumns) {
		t.Errorf("all-columns should win, got %v", got)
	}
	// --columns flag wins over config.
	if got := effectiveColumns(false, "pid,url", false, []string{"port"}); !reflect.DeepEqual(got, []string{"pid", "url"}) {
		t.Errorf("columns flag should win, got %v", got)
	}
	// config columns used when no flags, no stats.
	if got := effectiveColumns(false, "", false, []string{"port", "pid"}); !reflect.DeepEqual(got, []string{"port", "pid"}) {
		t.Errorf("config columns should be used, got %v", got)
	}
	// no flags, no config, no stats -> nil (RenderTable uses its defaults).
	if got := effectiveColumns(false, "", false, nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	// stats with config base appends stats columns to config columns.
	got := effectiveColumns(false, "", true, []string{"port"})
	want := []string{"port", "cpu", "mem", "state", "uptime", "connections"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stats+config columns = %v, want %v", got, want)
	}
}
