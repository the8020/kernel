package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/execution"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/workers"
)

func serviceEvents(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var entries []map[string]any
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return entries
		} else if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
}

func TestServiceLifecycleLogsTransitionsAndPoolIdentity(t *testing.T) {
	var output bytes.Buffer
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeWorkers{inFlight: map[string]int{}}
	m, err := New(&fakeCoordinator{}, runtime, store, Policy{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	options := testOptions(1, 1, 1)
	options.LogicalServiceID = "acme/billing/invoices"
	options.User, _ = execution.UserForUsername("alice")
	record, err := m.Start(context.Background(), "srv-0123456789", "file:///service.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	runtime.inFlight[record.WorkerIDs[0]] = 1
	for range 3 {
		if stopped, err := m.Stop(context.Background(), record.ServiceID); err != nil || stopped {
			t.Fatalf("occupied stop: %v %v", stopped, err)
		}
	}
	runtime.inFlight[record.WorkerIDs[0]] = 0
	for range 2 {
		if stopped, err := m.Stop(context.Background(), record.ServiceID); err != nil || !stopped {
			t.Fatalf("idle stop: %v %v", stopped, err)
		}
	}
	if err := m.RemoveStopped(record.ServiceID); err != nil {
		t.Fatal(err)
	}
	entries := serviceEvents(t, &output)
	want := []string{"service_starting", "service_started", "service_draining", "service_stopped", "service_removed"}
	if len(entries) != len(want) {
		t.Fatalf("repeated lifecycle transitions: %#v", entries)
	}
	for index, entry := range entries {
		if entry["event"] != want[index] || entry["service_id"] != record.ServiceID || entry["logical_service_id"] != options.LogicalServiceID || entry["username"] != "alice" || entry["component"] != "services" {
			t.Fatalf("pool attribution: %#v", entry)
		}
		if index > 0 && entry["sandbox_id"] != record.SandboxID {
			t.Fatalf("sandbox attribution: %#v", entry)
		}
		for _, unknown := range []string{"worker_id", "context_id", "parent_context_id", "service_pool_id"} {
			if _, present := entry[unknown]; present {
				t.Fatalf("pool event invented %s: %#v", unknown, entry)
			}
		}
	}
}

func TestServiceStartupFailureLogsBeforeReleasingSandbox(t *testing.T) {
	const secret = "request-private-value"
	var output bytes.Buffer
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &fakeCoordinator{}
	m, err := New(coordinator, &fakeWorkers{startErr: errors.New(secret)}, store, Policy{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	options := testOptions(1, 1, 1)
	options.LogicalServiceID = "acme/billing/invoices"
	record, err := m.Start(context.Background(), "srv-0123456789", "file:///service.ts", options)
	if err == nil || record.SandboxID != "" || len(coordinator.releases) != 1 {
		t.Fatalf("startup cleanup: %#v %v", record, err)
	}
	if _, err := m.Inspect(record.ServiceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup retained ownership: %v", err)
	}
	entries := serviceEvents(t, &output)
	if len(entries) != 2 || entries[1]["event"] != "service_failed" || entries[1]["sandbox_id"] != "sbx-0123456789" || entries[1]["service_id"] != record.ServiceID || entries[1]["level"] != "ERROR" {
		t.Fatalf("lost failed allocation: %#v", entries)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("pool event duplicated invocation error text")
	}
}

func TestServiceSandboxFailureRetainsObservedReasonAndDoesNotRepeat(t *testing.T) {
	var output bytes.Buffer
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(&fakeCoordinator{}, &fakeWorkers{}, store, Policy{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	options := testOptions(1, 1, 1)
	options.LogicalServiceID = "acme/billing/invoices"
	record, err := m.Start(context.Background(), "srv-0123456789", "file:///service.ts", options)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.FailSandbox(record.SandboxID, "sandbox exited with code 137"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Start(context.Background(), record.ServiceID, record.Entrypoint, options); err != nil {
		t.Fatal(err)
	}
	entries := serviceEvents(t, &output)
	if len(entries) != 5 || entries[2]["event"] != "service_failed" || entries[2]["reason"] != "sandbox exited with code 137" || entries[2]["service_id"] != record.ServiceID || entries[2]["sandbox_id"] != record.SandboxID || entries[4]["event"] != "service_started" || entries[4]["service_id"] != record.ServiceID {
		t.Fatalf("failed/restarted pool: %#v", entries)
	}
}

func TestServiceRecoveryLogsUseSharedAttribution(t *testing.T) {
	var output bytes.Buffer
	root := t.TempDir()
	store, err := records.New(root)
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("srv-0123456789")
	record.SandboxID = "sbx-0123456789"
	if err := store.Save(record.ServiceID, record); err != nil {
		t.Fatal(err)
	}
	const corruptID = "srv-abcdefghij"
	if err := os.WriteFile(filepath.Join(root, corruptID+".json"), []byte(`{"service_id":"srv-abcdefghij","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = records.New(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(&fakeCoordinator{}, &fakeWorkers{listErr: workers.ErrRuntimeUnavailable}, store, Policy{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	entries := serviceEvents(t, &output)
	if len(entries) != 2 {
		t.Fatalf("repeated recovery diagnostics: %#v", entries)
	}
	for _, entry := range entries {
		if entry["service_pool_id"] != nil || entry["level"] != "ERROR" {
			t.Fatalf("recovery violated shared log contract: %#v", entry)
		}
		switch entry["service_id"] {
		case record.ServiceID:
			if entry["sandbox_id"] != record.SandboxID || entry["logical_service_id"] != record.LogicalServiceID || entry["username"] != "system" {
				t.Fatalf("restore lost pool identity: %#v", entry)
			}
		case corruptID:
			if entry["event"] != "service_quarantined" || entry["sandbox_id"] != nil || entry["logical_service_id"] != nil || entry["username"] != nil {
				t.Fatalf("quarantine invented untrusted metadata: %#v", entry)
			}
		default:
			t.Fatalf("missing canonical service ID: %#v", entry)
		}
	}
}
