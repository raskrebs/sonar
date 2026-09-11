package hoststats

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/state"
)

// The parsers here carry no build tag, so the tests for every platform's
// source run on every platform.

// parseIoreg reads `ioreg -r -d 1 -c IOAccelerator`: one entry per GPU, each a
// `+-o <Class>  <class ...>` header followed by `"key" = value` lines. The two
// that matter are "model" and the PerformanceStatistics dictionary.
//
// Apple silicon reports "Device Utilization %" and "In use system memory";
// its memory is unified, so there is no total to report. A discrete card on an
// Intel Mac reports vramUsedBytes and vramFreeBytes, and older drivers call
// utilization "GPU Activity(%)". The result is never nil: no entries means
// ioreg answered and there is no GPU.
func parseIoreg(out string) []state.GPU {
	gpus := []state.GPU{}
	var cur *ioregEntry
	flush := func() {
		if cur != nil {
			gpus = append(gpus, cur.gpu())
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimLeft(line, " |")
		if strings.HasPrefix(trimmed, "+-o ") {
			flush()
			fields := strings.Fields(strings.TrimPrefix(trimmed, "+-o "))
			cur = &ioregEntry{}
			if len(fields) > 0 {
				cur.class = fields[0]
			}
			continue
		}
		if cur == nil {
			continue
		}
		key, value, ok := ioregProperty(strings.TrimSpace(trimmed))
		if !ok {
			continue
		}
		switch key {
		case "model":
			cur.model = ioregString(value)
		case "PerformanceStatistics":
			cur.stats = parseIoregDict(value)
		}
	}
	flush()
	return gpus
}

type ioregEntry struct {
	class string
	model string
	stats map[string]int64
}

func (e *ioregEntry) gpu() state.GPU {
	g := state.GPU{Name: e.model}
	if g.Name == "" {
		g.Name = e.class
	}
	if v, ok := firstKey(e.stats, "Device Utilization %", "GPU Activity(%)"); ok {
		g.UtilizationPercent = percent(float64(v))
	}
	if used, ok := e.stats["vramUsedBytes"]; ok && used >= 0 {
		g.MemoryUsedBytes = i64(used)
		if free, ok := e.stats["vramFreeBytes"]; ok && free >= 0 {
			g.MemoryTotalBytes = i64(used + free)
		}
	} else if used, ok := e.stats["In use system memory"]; ok && used >= 0 {
		g.MemoryUsedBytes = i64(used)
	}
	return g
}

func firstKey(m map[string]int64, keys ...string) (int64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return 0, false
}

// ioregProperty splits `"key" = value`.
func ioregProperty(line string) (key, value string, ok bool) {
	if !strings.HasPrefix(line, `"`) {
		return "", "", false
	}
	end := strings.IndexByte(line[1:], '"')
	if end < 0 {
		return "", "", false
	}
	key = line[1 : 1+end]
	rest := strings.TrimSpace(line[2+end:])
	if !strings.HasPrefix(rest, "=") {
		return "", "", false
	}
	return key, strings.TrimSpace(rest[1:]), true
}

// ioregString reads a string value, `"Apple M5 Pro"`, or the data form a PCI
// device's model takes, `<"AMD Radeon Pro 5500M">`.
func ioregString(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") {
		v = v[1 : len(v)-1]
	}
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return strings.TrimRight(v[1:len(v)-1], "\x00")
	}
	return ""
}

// parseIoregDict reads the integer values of a one-line ioreg dictionary,
// `{"a"=1,"b"="x","c"=(1,2),"d"={"e"=3}}`, into a map. Values that are not
// integers are skipped, nested ones without being descended into; a malformed
// dictionary returns what was read before the fault.
func parseIoregDict(s string) map[string]int64 {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	out := map[string]int64{}
	i := 1
	for i < len(s) {
		for i < len(s) && (s[i] == ',' || s[i] == ' ') {
			i++
		}
		if i >= len(s) || s[i] == '}' || s[i] != '"' {
			break
		}
		end := strings.IndexByte(s[i+1:], '"')
		if end < 0 {
			break
		}
		key := s[i+1 : i+1+end]
		i += end + 2
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			break
		}
		i++
		stop := skipIoregValue(s, i)
		if n, err := strconv.ParseInt(strings.TrimSpace(s[i:stop]), 10, 64); err == nil {
			out[key] = n
		}
		i = stop
	}
	return out
}

// skipIoregValue returns the index just past one value starting at i: the
// next comma or closing brace at depth zero, skipping quoted strings and
// nested dictionaries, arrays and data.
func skipIoregValue(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch s[i] {
		case '"':
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				return len(s)
			}
			i += end + 2
			continue
		case '{', '(', '<':
			depth++
		case '}', ')', '>':
			if depth == 0 {
				return i
			}
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return i
}

// nvidiaSMIArgs is the one query sonar asks nvidia-smi.
var nvidiaSMIArgs = []string{
	"--query-gpu=name,utilization.gpu,memory.used,memory.total",
	"--format=csv,noheader,nounits",
}

// parseNvidiaSMI reads nvidiaSMIArgs' output, one GPU per line:
//
//	NVIDIA GeForce RTX 4090, 35, 1024, 24564
//
// Memory is in MiB. A field the card cannot report comes back as "[N/A]" or
// "[Not Supported]" and becomes null. The name is everything before the last
// three fields, so a comma in a name cannot shift the numbers.
func parseNvidiaSMI(out string) ([]state.GPU, error) {
	gpus := []state.GPU{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		n := len(fields)
		if n < 4 {
			return nil, fmt.Errorf("nvidia-smi: unexpected line %q", line)
		}
		g := state.GPU{Name: strings.TrimSpace(strings.Join(fields[:n-3], ","))}
		if v, ok := csvNumber(fields[n-3]); ok {
			g.UtilizationPercent = percent(v)
		}
		if v, ok := csvNumber(fields[n-2]); ok && v >= 0 {
			g.MemoryUsedBytes = i64(int64(v * (1 << 20)))
		}
		if v, ok := csvNumber(fields[n-1]); ok && v >= 0 {
			g.MemoryTotalBytes = i64(int64(v * (1 << 20)))
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

func csvNumber(field string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// amdVendor is the PCI vendor id of AMD, as sysfs prints it.
const amdVendor = "0x1002"

// amdDevices lists the device directories of the amdgpu cards under a sysfs
// root ("/sys" in production). Connector entries (card0-DP-1) are skipped;
// only the card itself carries a device link.
func amdDevices(sysRoot string) []string {
	cards, _ := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*"))
	var devs []string
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue
		}
		dev := filepath.Join(card, "device")
		if v, ok := readTrim(filepath.Join(dev, "vendor")); ok && v == amdVendor {
			devs = append(devs, dev)
		}
	}
	return devs
}

// drmCards reports whether sysfs can be read and how many DRM cards it lists.
// A readable drm class with no cards at all is a machine with no GPU.
func drmCards(sysRoot string) (n int, readable bool) {
	dir := filepath.Join(sysRoot, "class", "drm")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "card") && !strings.Contains(name, "-") {
			n++
		}
	}
	return n, true
}

// readAMD reads the amdgpu counters of each device directory. Every file is
// optional: an older kernel lacks gpu_busy_percent, an APU may lack
// product_name, and a missing file is a null reading.
func readAMD(devs []string) []state.GPU {
	gpus := make([]state.GPU, 0, len(devs))
	for _, dev := range devs {
		g := state.GPU{}
		if name, ok := readTrim(filepath.Join(dev, "product_name")); ok && name != "" {
			g.Name = name
		} else if id, ok := readTrim(filepath.Join(dev, "device")); ok {
			g.Name = "AMD GPU " + id
		} else {
			g.Name = "AMD GPU"
		}
		if v, ok := readInt(filepath.Join(dev, "gpu_busy_percent")); ok {
			g.UtilizationPercent = percent(float64(v))
		}
		if v, ok := readInt(filepath.Join(dev, "mem_info_vram_used")); ok && v >= 0 {
			g.MemoryUsedBytes = i64(v)
		}
		if v, ok := readInt(filepath.Join(dev, "mem_info_vram_total")); ok && v > 0 {
			g.MemoryTotalBytes = i64(v)
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func readTrim(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func readInt(path string) (int64, bool) {
	s, ok := readTrim(path)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// percent rounds a utilization to whole percent and clamps it to 0–100. Every
// source counts in whole percent already; rounding here keeps a float source
// from publishing a `hosts` delta on the second decimal (see state.hostsEqual).
func percent(v float64) *float64 {
	v = math.Round(v)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return &v
}
