package state

import (
	"encoding/json"
	"strings"
	"testing"
)

func gpuRow(util float64, used int64) GPU {
	return GPU{Name: "Apple M5 Pro", UtilizationPercent: f64(util), MemoryUsedBytes: i64(used)}
}

// GPU memory on unified-memory machines moves by bytes on every read, and a
// driver may report fractional utilization. Neither is a change worth a delta;
// a whole percent or a whole MiB is.
func TestDiffComparesGPUsAtMeterResolution(t *testing.T) {
	base := hostRow("localhost")
	base.GPUs = []GPU{gpuRow(42, 1<<30)}

	for name, tc := range map[string]struct {
		gpus    []GPU
		changed bool
	}{
		"same":                {[]GPU{gpuRow(42, 1<<30)}, false},
		"sub-percent jitter":  {[]GPU{gpuRow(42.3, 1<<30)}, false},
		"sub-MiB jitter":      {[]GPU{gpuRow(42, 1<<30+4096)}, false},
		"a whole percent":     {[]GPU{gpuRow(43, 1<<30)}, true},
		"a whole MiB":         {[]GPU{gpuRow(42, 1<<30+1<<20)}, true},
		"to not collected":    {nil, true},
		"to no GPU":           {[]GPU{}, true},
		"a second GPU":        {[]GPU{gpuRow(42, 1<<30), gpuRow(0, 0)}, true},
		"utilization to null": {[]GPU{{Name: "Apple M5 Pro", MemoryUsedBytes: i64(1 << 30)}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			next := hostRow("localhost")
			next.GPUs = tc.gpus
			d := Diff(Snapshot{Hosts: []Host{base}}, Snapshot{Hosts: []Host{next}})
			if got := len(d.Hosts.Updated) == 1; got != tc.changed {
				t.Fatalf("changed = %v, want %v", got, tc.changed)
			}
		})
	}

	// Null to [] is news too: "not collected" became "no GPU".
	a, b := hostRow("localhost"), hostRow("localhost")
	b.GPUs = []GPU{}
	if d := Diff(Snapshot{Hosts: []Host{a}}, Snapshot{Hosts: []Host{b}}); len(d.Hosts.Updated) != 1 {
		t.Fatal("null to [] published no update")
	}
}

// gpusKey must not write through to the row it measures.
func TestGPUComparisonLeavesRowsAlone(t *testing.T) {
	a := hostRow("localhost")
	a.GPUs = []GPU{gpuRow(42.4, 1<<30+7)}
	b := a
	hostsEqual(a, b)
	if *a.GPUs[0].UtilizationPercent != 42.4 || *a.GPUs[0].MemoryUsedBytes != 1<<30+7 {
		t.Fatalf("comparison rounded the row itself: %+v", a.GPUs[0])
	}
}

// On the wire, null and [] are different answers and must stay so; every GPU
// reading is present, as null when unknown.
func TestHostGPUsWireForm(t *testing.T) {
	h := hostRow("localhost")
	raw, _ := json.Marshal(h)
	if !strings.Contains(string(raw), `"gpus":null`) {
		t.Errorf("uncollected GPUs marshal as %s, want null", raw)
	}

	h.GPUs = []GPU{}
	raw, _ = json.Marshal(h)
	if !strings.Contains(string(raw), `"gpus":[]`) {
		t.Errorf("no GPUs marshal as %s, want []", raw)
	}

	h.GPUs = []GPU{{Name: "Apple M5 Pro", UtilizationPercent: f64(21), MemoryUsedBytes: i64(1639071744)}}
	raw, _ = json.Marshal(h)
	want := `"gpus":[{"name":"Apple M5 Pro","utilization_percent":21,"memory_used_bytes":1639071744,"memory_total_bytes":null}]`
	if !strings.Contains(string(raw), want) {
		t.Errorf("GPU row marshals as %s, want %s", raw, want)
	}

	// A remote daemon too old to send the field decodes as not collected.
	var old Host
	if err := json.Unmarshal([]byte(`{"name":"hetzner","status":"connected"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.GPUs != nil {
		t.Errorf("a row without gpus decoded as %+v, want null", old.GPUs)
	}
	var none Host
	if err := json.Unmarshal([]byte(`{"name":"hetzner","gpus":[]}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.GPUs == nil {
		t.Error(`"gpus":[] decoded as null, want an empty list`)
	}
}
