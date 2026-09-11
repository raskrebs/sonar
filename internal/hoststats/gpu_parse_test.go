package hoststats

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/raskrebs/sonar/internal/state"
)

func wantGPU(t *testing.T, g state.GPU, name string, util *float64, used, total *int64) {
	t.Helper()
	if g.Name != name {
		t.Errorf("name = %q, want %q", g.Name, name)
	}
	checkF := func(field string, got, want *float64) {
		switch {
		case got == nil && want == nil:
		case got == nil || want == nil:
			t.Errorf("%s = %v, want %v", field, deref(got), deref(want))
		case *got != *want:
			t.Errorf("%s = %v, want %v", field, *got, *want)
		}
	}
	checkI := func(field string, got, want *int64) {
		switch {
		case got == nil && want == nil:
		case got == nil || want == nil:
			t.Errorf("%s = %v, want %v", field, derefI(got), derefI(want))
		case *got != *want:
			t.Errorf("%s = %d, want %d", field, *got, *want)
		}
	}
	checkF("utilization_percent", g.UtilizationPercent, util)
	checkI("memory_used_bytes", g.MemoryUsedBytes, used)
	checkI("memory_total_bytes", g.MemoryTotalBytes, total)
}

func f64(v float64) *float64 { return &v }

func deref(p *float64) any {
	if p == nil {
		return "null"
	}
	return *p
}

func derefI(p *int64) any {
	if p == nil {
		return "null"
	}
	return *p
}

// The fixture is `ioreg -r -d 1 -c IOAccelerator` from an Apple M5 Pro, kept
// whole: the long IOReportLegend line and the nested dictionaries are exactly
// what a careless parser trips on.
func TestParseIoregAppleSilicon(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "ioreg-apple-m5-pro.txt"))
	if err != nil {
		t.Fatal(err)
	}
	gpus := parseIoreg(string(data))
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1: %+v", len(gpus), gpus)
	}
	// Unified memory: the total is null, never invented.
	wantGPU(t, gpus[0], "Apple M5 Pro", f64(21), i64(1639071744), nil)
}

// An Intel Mac with a discrete card: VRAM counters, the older utilization key,
// and no model on the accelerator itself, so the class name stands in.
func TestParseIoregDiscreteCard(t *testing.T) {
	out := `+-o AMDRadeonX6000_AMDNavi14GraphicsAccelerator  <class AMDRadeonX6000_AMDNavi14GraphicsAccelerator, id 0x1000008d3, registered, matched, active, busy 0 (3 ms), retain 20>
    {
      "IOClass" = "AMDRadeonX6000_AMDNavi14GraphicsAccelerator"
      "PerformanceStatistics" = {"GPU Activity(%)"=37,"vramUsedBytes"=1073741824,"vramFreeBytes"=3221225472,"Some Array"=(1,2,3),"Nested"={"x"=1,"y"="a,b"}}
    }
+-o IntelAccelerator  <class IntelAccelerator, id 0x100000abc, registered, matched, active, busy 0 (1 ms), retain 12>
    {
      "model" = <"Intel UHD Graphics 630">
      "PerformanceStatistics" = {"Device Utilization %"=4,"In use system memory"=268435456}
    }
`
	gpus := parseIoreg(out)
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2: %+v", len(gpus), gpus)
	}
	wantGPU(t, gpus[0], "AMDRadeonX6000_AMDNavi14GraphicsAccelerator", f64(37), i64(1<<30), i64(4<<30))
	wantGPU(t, gpus[1], "Intel UHD Graphics 630", f64(4), i64(256<<20), nil)
}

// ioreg answering with nothing is "collected, no GPU": an empty list, not nil.
func TestParseIoregNoAccelerator(t *testing.T) {
	gpus := parseIoreg("")
	if gpus == nil || len(gpus) != 0 {
		t.Fatalf("parseIoreg(\"\") = %#v, want an empty non-nil list", gpus)
	}
}

// An accelerator with no statistics is still a GPU, with null readings.
func TestParseIoregWithoutStatistics(t *testing.T) {
	gpus := parseIoreg("+-o AGXAcceleratorG13X  <class AGXAcceleratorG13X>\n    {\n      \"model\" = \"Apple M1\"\n    }\n")
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(gpus))
	}
	wantGPU(t, gpus[0], "Apple M1", nil, nil, nil)
}

func TestParseIoregDict(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want map[string]int64
	}{
		"ints":        {`{"a"=1,"b"=22}`, map[string]int64{"a": 1, "b": 22}},
		"skip values": {`{"s"="x,}y","arr"=(1,{"q"=2}),"d"=<0a0b>,"n"=5}`, map[string]int64{"n": 5}},
		"negative":    {`{"a"=-3}`, map[string]int64{"a": -3}},
		"empty":       {`{}`, map[string]int64{}},
		"truncated":   {`{"a"=1,"b`, map[string]int64{"a": 1}},
		"not a dict":  {`"x"`, nil},
		"nested only": {`{"d"={"inner"=7}}`, map[string]int64{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseIoregDict(tc.in)
			if (got == nil) != (tc.want == nil) || len(got) != len(tc.want) {
				t.Fatalf("parseIoregDict(%s) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	out := "NVIDIA GeForce RTX 4090, 35, 1024, 24564\n" +
		"NVIDIA A100-SXM4-80GB, [N/A], [N/A], 81920\n" +
		"Tesla T4, 0, [Not Supported], [Unknown Error]\n" +
		"Odd, Name, With Commas, 99.6, 1, 2\n\n"
	gpus, err := parseNvidiaSMI(out)
	if err != nil {
		t.Fatalf("parseNvidiaSMI: %v", err)
	}
	if len(gpus) != 4 {
		t.Fatalf("got %d GPUs, want 4: %+v", len(gpus), gpus)
	}
	wantGPU(t, gpus[0], "NVIDIA GeForce RTX 4090", f64(35), i64(1024<<20), i64(24564<<20))
	wantGPU(t, gpus[1], "NVIDIA A100-SXM4-80GB", nil, nil, i64(81920<<20))
	wantGPU(t, gpus[2], "Tesla T4", f64(0), nil, nil)
	// Everything before the last three fields is the name; utilization rounds.
	wantGPU(t, gpus[3], "Odd, Name, With Commas", f64(100), i64(1<<20), i64(2<<20))
}

func TestParseNvidiaSMIEmptyAndMalformed(t *testing.T) {
	gpus, err := parseNvidiaSMI("")
	if err != nil || gpus == nil || len(gpus) != 0 {
		t.Fatalf("parseNvidiaSMI(\"\") = %#v, %v; want an empty list", gpus, err)
	}
	if _, err := parseNvidiaSMI("No devices were found\n"); err == nil {
		t.Fatal("a line that is not CSV parsed without error")
	}
}

// fakeSysfs lays out /sys/class/drm the way the kernel does, in a temp dir.
func fakeSysfs(t *testing.T, cards map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	drm := filepath.Join(root, "class", "drm")
	if err := os.MkdirAll(drm, 0o755); err != nil {
		t.Fatal(err)
	}
	for card, files := range cards {
		dev := filepath.Join(drm, card, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dev, name), []byte(content+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestReadAMDSysfs(t *testing.T) {
	root := fakeSysfs(t, map[string]map[string]string{
		"card0": {
			"vendor": "0x1002", "device": "0x744c", "product_name": "AMD Radeon RX 7900 XTX",
			"gpu_busy_percent": "57", "mem_info_vram_used": "2147483648", "mem_info_vram_total": "25753026560",
		},
		// A connector: carries the card's device link on a real system, and
		// must not be counted as a second card.
		"card0-DP-1": {"vendor": "0x1002"},
		"card1":      {"vendor": "0x8086", "device": "0x46a6"},
		// An older kernel: no product name, no busy counter.
		"card2": {"vendor": "0x1002", "device": "0x15d8", "mem_info_vram_used": "536870912"},
	})

	devs := amdDevices(root)
	if len(devs) != 2 {
		t.Fatalf("amdDevices = %v, want card0 and card2", devs)
	}
	gpus := readAMD(devs)
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(gpus))
	}
	wantGPU(t, gpus[0], "AMD Radeon RX 7900 XTX", f64(57), i64(2<<30), i64(25753026560))
	wantGPU(t, gpus[1], "AMD GPU 0x15d8", nil, i64(512<<20), nil)
}

func noTool(string) (string, error) { return "", errors.New("not found") }

func TestProbeLinuxGPUs(t *testing.T) {
	t.Run("no cards at all is no GPU", func(t *testing.T) {
		r := probeLinuxGPUs(noTool, fakeSysfs(t, nil))
		if r == nil {
			t.Fatal("an empty drm class probed as unsupported, want the no-GPU source")
		}
		gpus, err := r(t.Context())
		if err != nil || gpus == nil || len(gpus) != 0 {
			t.Fatalf("read = %#v, %v; want []", gpus, err)
		}
	})
	t.Run("intel only is unsupported", func(t *testing.T) {
		root := fakeSysfs(t, map[string]map[string]string{"card0": {"vendor": "0x8086"}})
		if r := probeLinuxGPUs(noTool, root); r != nil {
			t.Fatal("an Intel-only box probed as supported")
		}
	})
	t.Run("no sysfs is unsupported", func(t *testing.T) {
		if r := probeLinuxGPUs(noTool, filepath.Join(t.TempDir(), "missing")); r != nil {
			t.Fatal("a missing sysfs probed as supported")
		}
	})
	t.Run("amd card is read from sysfs", func(t *testing.T) {
		root := fakeSysfs(t, map[string]map[string]string{
			"card0": {"vendor": "0x1002", "product_name": "Radeon", "gpu_busy_percent": "3"},
		})
		r := probeLinuxGPUs(noTool, root)
		if r == nil {
			t.Fatal("an amdgpu card probed as unsupported")
		}
		gpus, err := r(t.Context())
		if err != nil || len(gpus) != 1 || gpus[0].Name != "Radeon" {
			t.Fatalf("read = %+v, %v", gpus, err)
		}
	})
	t.Run("nvidia-smi on PATH is a source", func(t *testing.T) {
		var asked []string
		look := func(name string) (string, error) {
			asked = append(asked, name)
			if name == "nvidia-smi" {
				return "/usr/bin/nvidia-smi", nil
			}
			return "", errors.New("not found")
		}
		if r := probeLinuxGPUs(look, filepath.Join(t.TempDir(), "missing")); r == nil {
			t.Fatal("nvidia-smi on PATH probed as unsupported")
		}
		if len(asked) != 1 || asked[0] != "nvidia-smi" {
			t.Fatalf("looked up %v, want only nvidia-smi", asked)
		}
	})
}

func TestProbeDarwinGPUs(t *testing.T) {
	if r := probeDarwinGPUs(noTool, "/nonexistent/ioreg"); r != nil {
		t.Fatal("no ioreg anywhere probed as supported")
	}
	look := func(name string) (string, error) {
		if name == "/usr/sbin/ioreg" {
			return name, nil
		}
		return "", errors.New("not found")
	}
	if r := probeDarwinGPUs(look, "/usr/sbin/ioreg"); r == nil {
		t.Fatal("ioreg at the fallback path probed as unsupported")
	}
}
