package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestStatusWorkersAndControlRoutes(t *testing.T) {
	var paths []string
	var messageTypes []protocol.MessageType
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		paths = append(paths, request.URL.RequestURI())
		writer.Header().Set("Content-Type", "application/json")
		var control protocol.Envelope
		if request.Method == http.MethodPost {
			var err error
			control, err = readControlRequest(request)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			messageTypes = append(messageTypes, control.MessageType)
		}
		switch request.URL.Path {
		case "/v1/status":
			_, _ = io.WriteString(writer, `{"protocol_version":1,"supervisor_version":"test","deno_version":"2.9.4","runtime_group_id":"group","sandbox_id":"sandbox","workload_type":"job","worker_count":2,"ready_worker_count":1,"failed_worker_count":1,"active_requests":1,"active_execution_count":1,"recent_failures":[{"worker_id":"failed-worker","execution_id":"failed-execution","reason":"boom"}]}`)
		case "/v1/workers":
			_, _ = io.WriteString(writer, `{"workers":[{"worker_id":"worker","execution_id":"execution","workload_id":"job","owner_id":"owner","debugger_name":"job:execution","in_flight":0,"idle_since_ms":1700000000000,"state":"failed","failure":"boom"}]}`)
		case "/v1/workers/start":
			var body StartWorkerRequest
			if err := json.Unmarshal(control.Payload, &body); err != nil || body.Metadata.WorkerID != "worker" {
				http.Error(writer, "bad body", http.StatusBadRequest)
				return
			}
			writeControlResponse(writer, control, protocol.MessageWorkerStateChange, map[string]any{"worker": map[string]any{"worker_id": "worker", "execution_id": "execution", "state": "ready"}}, http.StatusCreated)
		case "/v1/jobs/worker/run":
			writeControlResponse(writer, control, protocol.MessageJobResult, map[string]any{"result": map[string]any{"ok": true}, "logs": []map[string]any{{"level": "info", "message": "ran"}}}, http.StatusOK)
		case "/v1/workers/worker/invoke":
			writeControlResponse(writer, control, protocol.MessageWorkerResult, map[string]any{"ok": true, "output": "reply"}, http.StatusOK)
		case "/v1/services/service-a/configure":
			var body struct {
				WorkerIDs            []string `json:"worker_ids"`
				ConcurrencyPerWorker int      `json:"concurrency_per_worker"`
			}
			if err := json.Unmarshal(control.Payload, &body); err != nil || body.WorkerIDs == nil || len(body.WorkerIDs) != 0 || body.ConcurrencyPerWorker != 2 {
				http.Error(writer, "bad service configuration", http.StatusBadRequest)
				return
			}
			writeControlResponse(writer, control, protocol.MessageServicePoolConfiguration, map[string]any{"configured": true}, http.StatusOK)
		case "/v1/drain":
			writeControlResponse(writer, control, protocol.MessageRuntimeDrain, map[string]any{"draining": true}, http.StatusOK)
		default:
			writeControlResponse(writer, control, protocol.MessageWorkerStateChange, map[string]any{"stopped": true}, http.StatusOK)
		}
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	spec := testSpec()
	status, err := client.Status(context.Background(), spec)
	if err != nil || status.DenoVersion != "2.9.4" || status.ReadyWorkerCount != 1 || status.FailedWorkerCount != 1 || status.ActiveExecutionCount != 1 || len(status.RecentFailures) != 1 || status.RecentFailures[0].Reason != "boom" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	workers, err := client.Workers(context.Background(), spec)
	if err != nil || len(workers) != 1 || workers[0].WorkerID != "worker" || workers[0].IdleSinceMS != 1700000000000 || workers[0].State != "failed" || workers[0].Failure != "boom" {
		t.Fatalf("workers=%#v err=%v", workers, err)
	}
	request := StartWorkerRequest{Metadata: ExecutionMetadata{WorkerID: "worker", ExecutionID: "execution", WorkloadType: model.WorkloadJob, OwnerID: "owner", WorkloadID: "job", Entrypoint: "file:///artifacts/job.ts", DebuggerName: "job:execution"}}
	if worker, err := client.StartWorker(context.Background(), spec, request); err != nil || worker.WorkerID != "worker" {
		t.Fatalf("start=%#v err=%v", worker, err)
	}
	result, err := client.RunJob(context.Background(), spec, "worker", map[string]any{"value": 1}, nil)
	if err != nil || result.Result.(map[string]any)["ok"] != true || len(result.Logs) != 1 || result.Logs[0].Message != "ran" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if err := client.StopWorker(context.Background(), spec, "worker", true); err != nil {
		t.Fatal(err)
	}
	if output, err := client.InvokeWorker(context.Background(), spec, "worker", "example.inspect", map[string]any{"id": 1}); err != nil || !output.OK || output.Output != "reply" {
		t.Fatalf("Worker invocation=%#v err=%v", output, err)
	}
	if err := client.ConfigureService(context.Background(), spec, "service-a", nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := client.Drain(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !containsPath(paths, "/v1/workers/worker/stop?immediate=true") {
		t.Fatalf("paths=%#v", paths)
	}
	wantTypes := []protocol.MessageType{protocol.MessageStartWorker, protocol.MessageJobStart, protocol.MessageStopWorker, protocol.MessageWorkerInvoke, protocol.MessageServicePoolConfiguration, protocol.MessageRuntimeDrain}
	if len(messageTypes) != len(wantTypes) {
		t.Fatalf("message types=%#v", messageTypes)
	}
	for index := range wantTypes {
		if messageTypes[index] != wantTypes[index] {
			t.Fatalf("message type[%d]=%q want %q", index, messageTypes[index], wantTypes[index])
		}
	}
}

func readControlRequest(request *http.Request) (protocol.Envelope, error) {
	var message protocol.Envelope
	if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
		return message, err
	}
	if err := message.Validate(); err != nil {
		return message, err
	}
	if message.CorrelationID == "" {
		return message, context.Canceled
	}
	return message, nil
}

func writeControlResponse(writer http.ResponseWriter, request protocol.Envelope, messageType protocol.MessageType, payload any, status int) {
	payloadData, _ := json.Marshal(payload)
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(protocol.Envelope{ProtocolVersion: protocol.ProtocolVersion, MessageType: messageType, RuntimeGroupID: request.RuntimeGroupID, CorrelationID: request.CorrelationID, Payload: payloadData})
}

func TestStatusRejectsProtocolAndIdentityMismatch(t *testing.T) {
	tests := []string{
		`{"protocol_version":2,"runtime_group_id":"group","sandbox_id":"sandbox","workload_type":"job"}`,
		`{"protocol_version":1,"runtime_group_id":"other","sandbox_id":"sandbox","workload_type":"job"}`,
	}
	for _, response := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, response) }))
		client := testClient(t, server.URL)
		if _, err := client.Status(context.Background(), testSpec()); err == nil {
			t.Errorf("accepted response %s", response)
		}
		server.Close()
	}
}

func TestServiceDispatchStreamsRequestAndResponse(t *testing.T) {
	const body = "stream-one\nstream-two\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/services/service-a/dispatch" || request.Header.Get("X-80-20-Method") != "PATCH" || request.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(writer, "wrong request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(writer, request.Body)
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	original := httptest.NewRequest(http.MethodPatch, "http://public.example/upload", strings.NewReader(body))
	response, err := client.DispatchService(context.Background(), testSpec(), "service-a", original)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != body {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

func TestServiceDispatchReturnsRedirectWithoutFollowingPrivateSupervisorPath(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/services/service-a/dispatch" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Location", "/example/application/")
		writer.Header().Set("Set-Cookie", "the8020_auth=opaque; HttpOnly; Path=/")
		writer.Header().Set("X-80-20-Service-Response", "true")
		writer.WriteHeader(http.StatusSeeOther)
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	response, err := client.DispatchService(
		context.Background(),
		testSpec(),
		"service-a",
		httptest.NewRequest(http.MethodPost, "http://public.example/login", nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/example/application/" || response.Header.Get("Set-Cookie") == "" || requests != 1 {
		t.Fatalf("redirect response=%#v requests=%d", response, requests)
	}
}

func TestServiceWebSocketProxyPreservesOriginalURLAndAuthenticatesSupervisorHop(t *testing.T) {
	var received *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received = request.Clone(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	original := httptest.NewRequest(http.MethodGet, "http://public.example/core/events/echo?format=text", nil)
	original.Header.Set("Connection", "Upgrade")
	original.Header.Set("Upgrade", "websocket")
	original.Header.Set("Sec-WebSocket-Protocol", "the8020.echo")
	recorder := httptest.NewRecorder()
	if err := client.ProxyServiceWebSocket(context.Background(), testSpec(), "service-a", recorder, original); err != nil {
		t.Fatal(err)
	}
	if received == nil || received.URL.Path != "/v1/services/service-a/websocket" || received.URL.RawQuery != "format=text" {
		t.Fatalf("supervisor WebSocket target = %#v", received)
	}
	if received.Header.Get("Authorization") != "Bearer "+testToken || received.Header.Get("X-80-20-URL") != "http://public.example/core/events/echo?format=text" || received.Header.Get("Sec-WebSocket-Protocol") != "the8020.echo" {
		t.Fatalf("supervisor WebSocket headers = %#v", received.Header)
	}
	missingToken := testSpec()
	missingToken.InternalToken = ""
	if err := client.ProxyServiceWebSocket(context.Background(), missingToken, "service-a", httptest.NewRecorder(), original); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing WebSocket token error = %v", err)
	}
}

func TestRemoteErrorIsBoundedAndTokenIsRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, strings.Repeat("x", 20_000), http.StatusBadRequest)
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	if _, err := client.Workers(context.Background(), testSpec()); err == nil || len(err.Error()) > 9_000 || !IsRequestRejected(err) {
		t.Fatalf("bounded error=%v", err)
	}
	spec := testSpec()
	spec.InternalToken = ""
	if _, err := client.Workers(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("token error=%v", err)
	}
}

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := New(Config{ProtocolVersion: 1, HTTPClient: http.DefaultClient, Endpoint: func(model.SandboxSpec) (string, error) { return endpoint, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testSpec() model.SandboxSpec {
	return model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadJob, Network: model.NetworkConfiguration{SandboxIP: "10.88.0.2"}, InternalToken: testToken}
}

func containsPath(paths []string, target string) bool {
	for _, path := range paths {
		if path == target {
			return true
		}
	}
	return false
}
