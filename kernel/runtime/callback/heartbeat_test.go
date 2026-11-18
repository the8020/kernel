package callback

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

func TestSnapshotsAllowAdditiveFieldsWithoutChangingFreshnessRules(t *testing.T) {
	for _, workload := range []model.WorkloadType{model.WorkloadJob, model.WorkloadService} {
		t.Run(string(workload), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixtureForWorkload(t, store, workload)
			now := time.Unix(1700000000, 0).UTC()
			server := newCallbackTestServer(t, store, func(c *Config) { c.Now = func() time.Time { return now } })
			if response := runtimeControlCall(t, server, spec, "/v1/runtime/register", protocol.MessageSupervisorRegistration, statusPayload{Revision: 1, SupervisorStartedAtMS: 100, ProtocolVersion: protocol.ProtocolVersion, SandboxID: spec.SandboxID, WorkloadType: string(workload)}); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			payload := map[string]any{
				"revision": 2, "supervisor_started_at_ms": 100, "protocol_version": protocol.ProtocolVersion,
				"sandbox_id": spec.SandboxID, "workload_type": string(workload), "worker_count": 1,
				"future_snapshot": map[string]any{"value": []any{true, nil, "metadata"}},
				"workers": []any{map[string]any{
					"worker_id": "wrk-0000000001", "state": "ready", "future_worker": true,
					"resources": map[string]any{"cpu_usage_micros": 12, "heap_used_bytes": 34, "sampled_at": now, "future_resource": true},
				}},
				"recent_failures": []any{map[string]any{"worker_id": "wrk-0000000002", "reason": "fixture", "future_failure": true}},
			}
			for _, endpoint := range []struct {
				path string
				kind protocol.MessageType
			}{{"/v1/runtime/heartbeat", protocol.MessageHeartbeat}, {"/v1/runtime/register", protocol.MessageSupervisorRegistration}} {
				now = now.Add(5 * time.Second)
				response := runtimeControlCall(t, server, spec, endpoint.path, endpoint.kind, payload)
				if response.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", endpoint.path, response.Code, response.Body.String())
				}
				_, status, _ := store.Load(spec.SandboxID)
				snapshot, ok := store.Snapshot(spec.SandboxID)
				if !ok || !status.SupervisorHealthy || !status.LastHeartbeat.Equal(now) || snapshot.Revision != 2 || snapshot.WorkerCount != 1 || len(snapshot.Workers) != 1 || snapshot.Workers[0].Resources == nil || snapshot.Workers[0].Resources.CPUUsageMicros != 12 || snapshot.Workers[0].Resources.HeapUsedBytes != 34 || snapshot.Workers[0].Resources.SampledAt.IsZero() || len(snapshot.RecentFailures) != 1 {
					t.Fatalf("additive snapshot or equal-revision heartbeat lost: %+v %+v", status, snapshot)
				}
			}
			// Older epochs cannot renew health, even with a higher revision.
			acceptedAt := now
			now = now.Add(5 * time.Second)
			payload["supervisor_started_at_ms"], payload["revision"], payload["worker_count"] = 99, 3, 0
			if response := runtimeControlCall(t, server, spec, "/v1/runtime/heartbeat", protocol.MessageHeartbeat, payload); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			_, status, _ := store.Load(spec.SandboxID)
			snapshot, _ := store.Snapshot(spec.SandboxID)
			if !status.LastHeartbeat.Equal(acceptedAt) || snapshot.Revision != 2 || snapshot.WorkerCount != 1 {
				t.Fatalf("stale epoch renewed health or replaced state: %+v %+v", status, snapshot)
			}
		})
	}
}

func TestInvalidSnapshotsDoNotRenewHealth(t *testing.T) {
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"count type", "worker_count", "1"},
		{"revision overflow", "revision", json.Number("18446744073709551616")},
		{"missing revision", "revision", 0},
		{"protocol", "protocol_version", protocol.ProtocolVersion + 1},
		{"sandbox", "sandbox_id", "sbx-bbbbbbbbbb"},
		{"workload", "workload_type", "service"},
		{"negative cadence", "heartbeat_interval_ms", -1},
		{"excessive cadence", "heartbeat_interval_ms", 60001},
		{"fractional CPU", "workers", []any{map[string]any{"resources": map[string]any{"cpu_usage_micros": 1.25}}}},
		{"invalid date", "workers", []any{map[string]any{"resources": map[string]any{"sampled_at": "invalid"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixture(t, store)
			now := time.Unix(1700000000, 0).UTC()
			server := newCallbackTestServer(t, store, func(c *Config) { c.Now = func() time.Time { return now } })
			payload := map[string]any{
				"revision": 2, "supervisor_started_at_ms": 100, "protocol_version": protocol.ProtocolVersion,
				"sandbox_id": spec.SandboxID, "workload_type": string(spec.WorkloadType), "worker_count": 1,
			}
			if response := runtimeControlCall(t, server, spec, "/v1/runtime/register", protocol.MessageSupervisorRegistration, payload); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			now = now.Add(20 * time.Second)
			payload["future_snapshot"], payload[test.field] = true, test.value
			response := runtimeControlCall(t, server, spec, "/v1/runtime/heartbeat", protocol.MessageHeartbeat, payload)
			want := http.StatusBadRequest
			if test.name == "missing revision" {
				want = http.StatusInternalServerError
			}
			if response.Code != want {
				t.Fatalf("invalid snapshot response: %d %s", response.Code, response.Body.String())
			}
			_, status, _ := store.Load(spec.SandboxID)
			snapshot, _ := store.Snapshot(spec.SandboxID)
			if !status.LastHeartbeat.Equal(now.Add(-20*time.Second)) || snapshot.WorkerCount != 1 || snapshot.Revision != 2 {
				t.Fatal("invalid snapshot refreshed health or replaced state")
			}
		})
	}
}

func TestAdditiveSnapshotsCannotBypassAuthenticationOrTerminalState(t *testing.T) {
	for _, terminal := range []model.SandboxState{"", model.StateFailed, model.StateStopping, model.StateStopped, model.StateDeleting} {
		t.Run(string(terminal), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixture(t, store)
			now := time.Unix(1700000000, 0).UTC()
			server := newCallbackTestServer(t, store, func(c *Config) { c.Now = func() time.Time { return now } })
			payload := map[string]any{
				"revision": 2, "supervisor_started_at_ms": 100, "protocol_version": protocol.ProtocolVersion,
				"sandbox_id": spec.SandboxID, "workload_type": string(spec.WorkloadType), "worker_count": 1,
				"future_snapshot": true,
			}
			if response := runtimeControlCall(t, server, spec, "/v1/runtime/register", protocol.MessageSupervisorRegistration, payload); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			want := http.StatusUnauthorized
			if terminal == "" {
				spec.InternalToken = "incorrect-token"
			} else {
				_, status, _ := store.Load(spec.SandboxID)
				status.ObservedState, status.SupervisorHealthy = terminal, false
				if err := store.SaveStatus(spec.SandboxID, status); err != nil {
					t.Fatal(err)
				}
				want = http.StatusConflict
			}
			now = now.Add(20 * time.Second)
			payload["revision"], payload["worker_count"] = 3, 0
			response := runtimeControlCall(t, server, spec, "/v1/runtime/heartbeat", protocol.MessageHeartbeat, payload)
			_, status, _ := store.Load(spec.SandboxID)
			snapshot, _ := store.Snapshot(spec.SandboxID)
			if response.Code != want || !status.LastHeartbeat.Equal(now.Add(-20*time.Second)) || snapshot.Revision != 2 || snapshot.WorkerCount != 1 || (terminal != "" && status.SupervisorHealthy) {
				t.Fatalf("unauthorized/terminal snapshot changed health: %d %+v %+v", response.Code, status, snapshot)
			}
		})
	}
}

func TestSnapshotCompatibilityKeepsEnvelopeAndCommandDecodersStrict(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixture(t, store)
	server := newCallbackTestServer(t, store, nil)
	body := callbackMessage(t, protocol.MessageHeartbeat, spec, statusPayload{Revision: 2, ProtocolVersion: protocol.ProtocolVersion, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType)})
	for _, invalid := range [][]byte{
		append(append([]byte{}, body[:len(body)-1]...), []byte(`,"future_envelope":true}`)...),
		append(append([]byte{}, body...), []byte(` {}`)...),
		bytes.Replace(body, []byte(`"worker_count":0`), []byte(`"worker_count":!`), 1),
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/runtime/heartbeat", bytes.NewReader(invalid))
		request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
		response := httptest.NewRecorder()
		server.serveHTTP(response, request)
		_, status, _ := store.Load(spec.SandboxID)
		if response.Code != http.StatusBadRequest || !status.LastHeartbeat.IsZero() {
			t.Fatalf("invalid envelope accepted or renewed health: %d %+v", response.Code, status)
		}
	}
	for _, output := range []any{&adminCallPayload{}, &databaseCallPayload{}, &workerCallPayload{}, &operationCallPayload{}} {
		if err := decodePayload(json.RawMessage(`{"future_command":true}`), output); err == nil {
			t.Fatalf("unknown command field accepted for %T", output)
		}
		if err := decodePayload(json.RawMessage(`{} {}`), output); err == nil {
			t.Fatalf("trailing command data accepted for %T", output)
		}
	}
}
