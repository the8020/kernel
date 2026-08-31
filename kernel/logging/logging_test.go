package logging

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/settings"
)

func TestPeriodBoundaries(t *testing.T) {
	now := time.Date(2026, time.August, 19, 22, 17, 31, 0, time.UTC)
	tests := map[string]time.Time{"minute": time.Date(2026, 8, 19, 22, 18, 0, 0, time.UTC), "hour": time.Date(2026, 8, 19, 23, 0, 0, 0, time.UTC), "day": time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), "week": time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), "month": time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "year": time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	for period, want := range tests {
		if got := periodBoundary(now, period); !got.Equal(want) {
			t.Errorf("%s boundary = %s, want %s", period, got, want)
		}
	}
	if got := periodBoundary(now, "none"); !got.IsZero() {
		t.Errorf("none boundary = %s", got)
	}
}

func TestEnabledRotationAndTotalCleanup(t *testing.T) {
	directory := t.TempDir()
	manager, err := New(directory, Policy{Enabled: true, SplitPeriod: "day", MaxFileSize: 20, MaxTotalSize: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if !manager.Enabled() || manager.ActiveFile() == "" {
		t.Fatal("logging not enabled with active file")
	}
	first := manager.ActiveFile()
	for i := 0; i < 3; i++ {
		if _, err := manager.Write([]byte("123456789012345\n")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected oldest closed file cleanup, got %d files", len(entries))
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("oldest closed file was not deleted first: %v", err)
	}
	active := manager.ActiveFile()
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active file removed: %v", err)
	}
}

func loggingValues(enabled bool, file, total int64) settings.Values {
	return settings.Values{"logging.enabled": enabled, "logging.split_period": "day", "logging.max_file_size": settings.ByteSize(file), "logging.max_total_size": settings.ByteSize(total)}
}
func TestDisableReenableAndFailedReplacement(t *testing.T) {
	manager, err := New(t.TempDir(), Policy{Enabled: true, SplitPeriod: "day", MaxFileSize: 100, MaxTotalSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	original := manager.ActiveFile()
	disable, err := manager.Prepare(context.Background(), loggingValues(false, 100, 200))
	if err != nil {
		t.Fatal(err)
	}
	disable.Commit()
	if manager.Enabled() || manager.ActiveFile() != "" {
		t.Fatal("logging remained enabled")
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("existing log changed on disable: %v", err)
	}
	enable, err := manager.Prepare(context.Background(), loggingValues(true, 100, 200))
	if err != nil {
		t.Fatal(err)
	}
	enable.Commit()
	if !manager.Enabled() || manager.ActiveFile() == "" {
		t.Fatal("logging did not re-enable")
	}
	active := manager.ActiveFile()
	if _, err := manager.Prepare(context.Background(), loggingValues(true, 200, 100)); err == nil {
		t.Fatal("accepted invalid policy")
	}
	if manager.ActiveFile() != active || !manager.Enabled() {
		t.Fatal("failed replacement changed active policy")
	}
}

func TestTimeSplitAndDiscardRemovesPreparedFile(t *testing.T) {
	directory := t.TempDir()
	manager, err := New(directory, Policy{Enabled: true, SplitPeriod: "minute", MaxFileSize: 100, MaxTotalSize: 300})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	manager.mu.Lock()
	manager.now = func() time.Time { return now }
	manager.state.periodEnd = now.Add(10 * time.Second)
	manager.mu.Unlock()
	old := manager.ActiveFile()
	now = now.Add(time.Minute)
	if _, err := manager.Write([]byte("record\n")); err != nil {
		t.Fatal(err)
	}
	if manager.ActiveFile() == old {
		t.Fatal("period boundary did not split")
	}
	before, _ := filepath.Glob(filepath.Join(directory, "kernel-*.log"))
	prepared, err := manager.Prepare(context.Background(), loggingValues(true, 100, 300))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Discard()
	after, _ := filepath.Glob(filepath.Join(directory, "kernel-*.log"))
	if len(after) != len(before) {
		t.Fatalf("discard left prepared file: before %d after %d", len(before), len(after))
	}
}
