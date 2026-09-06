package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/sandbox/model"
)

func TestArchiveListInspectAndCleanup(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 27, 13, 4, 5, 123456789, time.UTC)
	store, err := New(Config{
		Root: filepath.Join(root, "history"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first := model.SandboxSpec{SandboxID: "sbx-ax9thsl300", WorkloadType: model.WorkloadService}
	createdAt := now.Add(-time.Minute)
	record, err := store.Archive(first, model.SandboxStatus{
		ObservedState: model.StateFailed, FailureReason: "heartbeat timeout",
		NodeID: "nod-abcdefghij", CreatedAt: createdAt, LogPosition: "saved-log-position",
	}, "heartbeat timeout", DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	second := model.SandboxSpec{SandboxID: "sbx-bbbbbbbbbb", WorkloadType: model.WorkloadJob}
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
	if inspection.Record.SchemaVersion != 2 || inspection.Record.Status.NodeID != "nod-abcdefghij" || inspection.Record.Status.LogPosition != "saved-log-position" || !inspection.Record.Status.CreatedAt.Equal(createdAt) {
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

func TestHistoryRetentionNeverTouchesUnifiedLogs(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "unified.log")
	if err := os.WriteFile(logPath, []byte("terminal diagnostics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store, err := New(Config{Root: filepath.Join(root, "history"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Archive(model.SandboxSpec{SandboxID: "sbx-cccccccccc"}, model.SandboxStatus{ObservedState: model.StateFailed}, "failed", DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := store.recordDirectory(record.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "metadata.json" {
		t.Fatalf("archive must contain only metadata: %v, %v", entries, err)
	}
	now = now.Add(DefaultRetention + 2*time.Hour)
	if removed, err := store.Cleanup(DefaultRetention); err != nil || removed != 1 {
		t.Fatalf("cleanup = %d, %v", removed, err)
	}
	if content, err := os.ReadFile(logPath); err != nil || string(content) != "terminal diagnostics\n" {
		t.Fatalf("history changed unified logs: %q, %v", content, err)
	}
}
