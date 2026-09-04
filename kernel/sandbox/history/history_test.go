package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/sandbox/model"
)

func TestArchiveListInspectAndCleanup(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "live-logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 13, 4, 5, 123456789, time.UTC)
	store, err := New(Config{
		Root: filepath.Join(root, "history"),
		Now:  func() time.Time { return now },
		LogPath: func(sandboxID string) string {
			return filepath.Join(logs, sandboxID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := model.SandboxSpec{SandboxID: "sbx-ax9thsl3", RuntimeGroupID: "group-one", WorkloadType: model.WorkloadService}
	firstLogs := filepath.Join(logs, first.SandboxID)
	if err := os.MkdirAll(firstLogs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstLogs, "runsc.log"), []byte("terminal diagnostics"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := store.Archive(first, model.SandboxStatus{ObservedState: model.StateFailed, FailureReason: "heartbeat timeout"}, "heartbeat timeout", DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	second := model.SandboxSpec{SandboxID: "sbx-bbbbbbbb", RuntimeGroupID: "group-two", WorkloadType: model.WorkloadJob}
	if _, err := store.Archive(second, model.SandboxStatus{ObservedState: model.StateDeleting}, "sandbox deleted", DefaultRetention); err != nil {
		t.Fatal(err)
	}

	page, err := store.List(1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sandboxes) != 1 || page.Sandboxes[0].SandboxID != second.SandboxID || page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	next, err := store.List(1, page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Sandboxes) != 1 || next.Sandboxes[0].SandboxID != first.SandboxID {
		t.Fatalf("next page = %#v", next)
	}
	inspection, err := store.Inspect(record.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Logs) != 1 || !strings.Contains(inspection.Logs[0].Content, "terminal diagnostics") {
		t.Fatalf("inspection = %#v", inspection)
	}
	if retained, err := store.ContainsSandboxID(first.SandboxID); err != nil || !retained {
		t.Fatalf("retained=%t err=%v", retained, err)
	}
	reloaded, err := New(Config{Root: filepath.Join(root, "history")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reloaded.markerPath(first.SandboxID)); err != nil {
		t.Fatal(err)
	}
	if retained, err := reloaded.ContainsSandboxID(first.SandboxID); err != nil || !retained {
		t.Fatalf("preloaded retained index performed a live stat: retained=%t err=%v", retained, err)
	}

	now = now.Add(DefaultRetention + 2*time.Hour)
	removed, err := store.Cleanup(DefaultRetention)
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if retained, err := store.ContainsSandboxID(first.SandboxID); err != nil || retained {
		t.Fatalf("retained after cleanup=%t err=%v", retained, err)
	}
}

func TestInspectBoundsLogTails(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "runtime.log")
	content := strings.Repeat("a", maximumLogBytes+100)
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(Config{Root: filepath.Join(root, "history"), LogPath: func(string) string { return logPath }})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Archive(model.SandboxSpec{SandboxID: "sbx-cccccccc", RuntimeGroupID: "group-three"}, model.SandboxStatus{ObservedState: model.StateFailed}, "failed", DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(record.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Logs) != 1 || !inspection.Logs[0].Truncated || len(inspection.Logs[0].Content) != maximumLogBytes {
		t.Fatalf("logs = %#v", inspection.Logs)
	}
}
