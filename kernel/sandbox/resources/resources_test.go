package resources

import (
	"os"
	"path/filepath"
	"testing"

	"the8020/kernel/sandbox/model"
)

func testLimits() model.ResourceLimits {
	return model.ResourceLimits{MemoryHigh: 128, MemoryMaximum: 256, SwapMaximum: 0, CPUQuotaMicros: 50_000, CPUPeriodMicros: 100_000, CPUWeight: 200, PIDMaximum: 64, TmpfsMaximum: 32}
}

func TestUnifiedSettings(t *testing.T) {
	settings, err := UnifiedSettings(testLimits())
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{"memory.high": "128", "memory.max": "256", "memory.swap.max": "0", "memory.oom.group": "1", "cpu.max": "50000 100000", "cpu.weight": "200", "pids.max": "64"}
	for key, want := range wants {
		if settings[key] != want {
			t.Errorf("%s = %q, want %q", key, settings[key], want)
		}
	}
}

func TestReadMetrics(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"memory.current": "101\n", "memory.peak": "202\n", "pids.current": "3\n",
		"memory.events": "low 1\nhigh 2\noom 3\noom_kill 1\n", "pids.events": "max 4\n",
		"cpu.stat": "usage_usec 55\nuser_usec 40\nsystem_usec 15\n", "cgroup.events": "populated 1\nfrozen 0\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := ReadMetrics(directory)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.MemoryCurrent != 101 || metrics.MemoryPeak != 202 || metrics.PIDCurrent != 3 || metrics.CPUUsageMicros != 55 || metrics.MemoryEvents["oom_kill"] != 1 || metrics.PIDEvents["max"] != 4 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestReadMetricsAllowsAbsentPeakButRejectsMalformedData(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{"memory.current": "1", "pids.current": "2", "memory.events": "oom 0", "pids.events": "max 0", "cpu.stat": "usage_usec 3", "cgroup.events": "populated 1"}
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600)
	}
	if _, err := ReadMetrics(directory); err != nil {
		t.Fatalf("absent optional peak: %v", err)
	}
	_ = os.WriteFile(filepath.Join(directory, "cpu.stat"), []byte("broken"), 0o600)
	if _, err := ReadMetrics(directory); err == nil {
		t.Fatal("accepted malformed cpu.stat")
	}
}
