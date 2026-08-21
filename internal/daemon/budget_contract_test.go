package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const defaultBudgetMinutes = 2

type budgetRun struct {
	minutes int
	full    bool
}

func parseBudgetRun(raw string) (budgetRun, error) {
	if raw == "" {
		return budgetRun{minutes: defaultBudgetMinutes}, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return budgetRun{}, fmt.Errorf("OONFEE_BUDGET_MINUTES must be a positive integer, got %q", raw)
	}
	return budgetRun{minutes: n, full: n >= 60}, nil
}

type cpuCounters struct {
	idle  uint64
	total uint64
}

func parseCPUCounters(line string) (cpuCounters, error) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuCounters{}, fmt.Errorf("malformed /proc/stat cpu line %q", line)
	}
	// Linux accounts guest time inside user/nice, so only the first eight
	// counters belong in the total. iowait is idle for utilization purposes.
	limit := len(f) - 1
	if limit > 8 {
		limit = 8
	}
	values := make([]uint64, limit)
	for i := range values {
		n, err := strconv.ParseUint(f[i+1], 10, 64)
		if err != nil {
			return cpuCounters{}, fmt.Errorf("malformed /proc/stat counter %q: %w", f[i+1], err)
		}
		values[i] = n
	}
	var total uint64
	for _, n := range values {
		total += n
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuCounters{idle: idle, total: total}, nil
}

func cpuBusyPercent(before, after cpuCounters) (float64, bool) {
	if after.total <= before.total || after.idle < before.idle {
		return 0, false
	}
	total := after.total - before.total
	idle := after.idle - before.idle
	if idle > total {
		return 0, false
	}
	return float64(total-idle) * 100 / float64(total), true
}

type memoryCounters struct {
	totalKB     int64
	availableKB int64
}

func parseMemoryCounters(text string) (memoryCounters, error) {
	values := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		key := strings.TrimSuffix(f[0], ":")
		switch key {
		case "MemTotal", "MemAvailable", "MemFree", "Buffers", "Cached", "SReclaimable":
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return memoryCounters{}, fmt.Errorf("malformed /proc/meminfo value %q: %w", f[1], err)
			}
			values[key] = n
		}
	}
	total := values["MemTotal"]
	if total <= 0 {
		return memoryCounters{}, fmt.Errorf("/proc/meminfo has no positive MemTotal")
	}
	available := values["MemAvailable"]
	if available == 0 {
		// Older OpenWrt kernels may not expose MemAvailable. This is the usual
		// conservative approximation; it remains a whole-device observation.
		available = values["MemFree"] + values["Buffers"] + values["Cached"] + values["SReclaimable"]
	}
	if available < 0 || available > total {
		return memoryCounters{}, fmt.Errorf("/proc/meminfo available memory %d KiB is outside 0..%d", available, total)
	}
	return memoryCounters{totalKB: total, availableKB: available}, nil
}

func (m memoryCounters) usedKB() int64 { return m.totalKB - m.availableKB }

func TestParseBudgetRun(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		wantMinutes int
		wantFull    bool
		wantErr     bool
	}{
		{"", 2, false, false},
		{"1", 1, false, false},
		{"59", 59, false, false},
		{"60", 60, true, false},
		{"90", 90, true, false},
		{"0", 0, false, true},
		{"abc", 0, false, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseBudgetRun(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseBudgetRun(%q) error = %v", tc.raw, err)
			}
			if got.minutes != tc.wantMinutes || got.full != tc.wantFull {
				t.Fatalf("parseBudgetRun(%q) = %+v, want minutes=%d full=%v",
					tc.raw, got, tc.wantMinutes, tc.wantFull)
			}
		})
	}
}

func TestResourceParsers(t *testing.T) {
	cpu, err := parseCPUCounters("cpu  100 10 20 800 20 5 5 0 30 4")
	if err != nil {
		t.Fatal(err)
	}
	if cpu.total != 960 || cpu.idle != 820 {
		t.Fatalf("CPU counters = %+v, want total=960 idle=820", cpu)
	}
	if pct, ok := cpuBusyPercent(cpu, cpuCounters{total: 1060, idle: 900}); !ok || pct != 20 {
		t.Fatalf("busy = %v, %v; want 20, true", pct, ok)
	}

	mem, err := parseMemoryCounters("MemTotal: 128000 kB\nMemAvailable: 48000 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := mem.usedKB(); got != 80000 {
		t.Fatalf("used memory = %d KiB, want 80000", got)
	}
	legacy, err := parseMemoryCounters("MemTotal: 128000 kB\nMemFree: 10000 kB\nBuffers: 2000 kB\nCached: 30000 kB\nSReclaimable: 1000 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.usedKB(); got != 85000 {
		t.Fatalf("legacy used memory = %d KiB, want 85000", got)
	}
}
