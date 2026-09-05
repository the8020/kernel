package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/execution"
	"the8020/kernel/nodes"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

type recordingWorkerInvoker struct {
	calls  []nodes.WorkerInvocationRequest
	result nodes.WorkerInvocationResult
}

func (i *recordingWorkerInvoker) InvokeWorker(_ context.Context, input nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult {
	i.calls = append(i.calls, input)
	return i.result
}

type recordingOperations struct {
	operation string
	input     map[string]any
	result    any
	err       error
	caller    execution.Caller
}

func (o *recordingOperations) Execute(ctx context.Context, operation string, input map[string]any) (any, error) {
	o.operation, o.input = operation, input
	o.caller, _ = execution.CallerFromContext(ctx)
	return o.result, o.err
}

func newCallbackTestServer(t testing.TB, store *state.Store, configure func(*Config)) *Server {
	t.Helper()
	config := Config{
		Store: store, ProtocolVersion: protocol.ProtocolVersion,
		SocketPath: filepath.Join(t.TempDir(), "kernel.sock"),
	}
	if configure != nil {
		configure(&config)
	}
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type recordingDatabase struct {
	mu         sync.Mutex
	queries    []string
	executions []string
	parameters []any
	statements []database.StatementRequest
	closed     []string
	prefixes   []string
	entered    chan struct{}
	release    chan struct{}
}

func (d *recordingDatabase) Status() database.Status {
	return database.Status{Backend: database.BackendSQLite, State: database.StateReady, MaximumResultBytes: database.DefaultMaximumResultBytes}
}

func (d *recordingDatabase) Query(_ context.Context, statement string, parameters []any) (database.QueryResult, error) {
	d.queries, d.parameters = append(d.queries, statement), parameters
	return database.QueryResult{Columns: []string{"value"}, Rows: [][]any{{int64(3)}}}, nil
}

func (d *recordingDatabase) Execute(_ context.Context, statement string, parameters []any) (database.ExecuteResult, error) {
	d.executions, d.parameters = append(d.executions, statement), parameters
	return database.ExecuteResult{RowsAffected: 1}, nil
}

func (d *recordingDatabase) RunStatement(_ context.Context, _ string, request database.StatementRequest) (database.StatementResult, error) {
	if d.entered != nil {
		d.entered <- struct{}{}
	}
	if d.release != nil {
		<-d.release
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statements = append(d.statements, request)
	d.executions = append(d.executions, request.Statement)
	return database.StatementResult{Columns: []string{}, Rows: [][]any{}, AffectedRows: map[string]any{"type": "bigint", "value": "1"}}, nil
}

func (d *recordingDatabase) BeginTransaction(context.Context, string, database.TransactionSettings) (string, error) {
	return "transaction-1", nil
}

func (d *recordingDatabase) FinishTransaction(context.Context, string, string, bool) error {
	return nil
}

func (d *recordingDatabase) CloseScope(scope string) { d.closed = append(d.closed, scope) }

func (d *recordingDatabase) CloseScopePrefix(scope string) { d.prefixes = append(d.prefixes, scope) }

func TestAuthenticatedRegistrationAndHeartbeatUpdateMemoryOnly(t *testing.T) {
	root := t.TempDir()
	store, _ := state.New(root)
	spec, status := callbackFixture(t, store)
	now := time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC)
	server := newCallbackTestServer(t, store, func(config *Config) { config.Now = func() time.Time { return now } })
	for _, endpoint := range []struct{ path, message string }{{"/v1/runtime/register", "supervisor_registration"}, {"/v1/runtime/heartbeat", "heartbeat"}} {
		body := callbackMessage(t, protocol.MessageType(endpoint.message), spec, statusPayload{ProtocolVersion: protocol.ProtocolVersion, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType), SupervisorVersion: "1.0.0", DenoVersion: "2.9.4", WorkerCount: 2})
		request := httptest.NewRequest(http.MethodPost, "http://callback"+endpoint.path, bytes.NewReader(body))
		request.RemoteAddr = "10.88.0.2:1000"
		request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
		response := httptest.NewRecorder()
		server.serveHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", endpoint.path, response.Code, response.Body.String())
		}
	}
	_, updated, err := store.Load(spec.RuntimeGroupID)
	if err != nil || !updated.SupervisorHealthy || updated.WorkerCount != 2 || !updated.LastHeartbeat.Equal(now) || updated.ObservedState != status.ObservedState {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	reloaded, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, durable, err := reloaded.Load(spec.RuntimeGroupID)
	if err != nil || durable.WorkerCount != status.WorkerCount || durable.SupervisorHealthy != status.SupervisorHealthy {
		t.Fatalf("runtime snapshot leaked into durable status: %#v err=%v", durable, err)
	}
}

func TestCallbackRejectsTokenAndProtocolMismatch(t *testing.T) {
	root := t.TempDir()
	store, _ := state.New(root)
	spec, _ := callbackFixture(t, store)
	server := newCallbackTestServer(t, store, nil)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	validPayload := statusPayload{Revision: 2, SupervisorStartedAtMS: 1, ProtocolVersion: protocol.ProtocolVersion, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType)}
	for _, test := range []struct {
		name, remote, token string
		version             int
		want                int
	}{
		{"token", "10.88.0.2:1", "wrong", protocol.ProtocolVersion, http.StatusUnauthorized},
		{"protocol", "10.88.0.2:1", spec.InternalToken, protocol.ProtocolVersion + 1, http.StatusBadRequest},
		{"cached valid token", "10.88.0.2:1", spec.InternalToken, protocol.ProtocolVersion, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, _ := json.Marshal(validPayload)
			body, _ := json.Marshal(protocol.Envelope{ProtocolVersion: test.version, MessageType: protocol.MessageHeartbeat, RuntimeGroupID: spec.RuntimeGroupID, Payload: payload})
			request := httptest.NewRequest(http.MethodPost, "http://callback/v1/runtime/heartbeat", bytes.NewReader(body))
			request.RemoteAddr = test.remote
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			server.serveHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestCallbackCannotReviveTerminalRuntimeGroup(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, status := callbackFixture(t, store)
	status.ObservedState = model.StateFailed
	status.SupervisorHealthy = false
	if err := store.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		t.Fatal(err)
	}
	server := newCallbackTestServer(t, store, nil)
	body := callbackMessage(t, protocol.MessageHeartbeat, spec, statusPayload{ProtocolVersion: protocol.ProtocolVersion, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType)})
	request := httptest.NewRequest(http.MethodPost, "http://callback/v1/runtime/heartbeat", bytes.NewReader(body))
	request.RemoteAddr = "10.88.0.2:1"
	request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	response := httptest.NewRecorder()
	server.serveHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	_, updated, _ := store.Load(spec.RuntimeGroupID)
	if updated.SupervisorHealthy || updated.ObservedState != model.StateFailed {
		t.Fatalf("updated=%#v", updated)
	}
}

func TestRemovedApplicationAuthenticationEndpoints(t *testing.T) {
	store, _ := state.New(t.TempDir())
	server := newCallbackTestServer(t, store, nil)
	for _, path := range []string{"/v1/runtime/auth/login", "/v1/runtime/auth/logout-current"} {
		response := httptest.NewRecorder()
		server.serveHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed endpoint returned %d", response.Code)
		}
	}
}

func TestJobsAndServicesCanExecuteAdminCommandsWithoutAUserOrHTTPRequest(t *testing.T) {
	for _, workload := range []model.WorkloadType{model.WorkloadJob, model.WorkloadService} {
		t.Run(string(workload), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixtureForWorkload(t, store, workload)
			registry := core.NewRegistry(nil)
			mutated := false
			err := registry.Register(core.Command{
				Version: 1, ID: "kernel.test", Name: "kernel.test", Path: []string{"kernel.test"},
				Parameters: []core.Parameter{{Name: "value", Type: "string", Required: true}},
			}, func(_ context.Context, request core.Request) (core.Result, error) {
				mutated = request.Arguments["value"] == "accepted"
				return core.Result{"state": "done"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			server := newCallbackTestServer(t, store, func(config *Config) {
				config.AdminBus = registry
			})
			payload := adminCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: "request-1",
				CommandID: "kernel.test", Arguments: map[string]any{"value": "accepted"},
				User: execution.SystemUser(),
			}
			response := runtimeControlCall(t, server, spec, "/v1/runtime/admin/execute", protocol.MessageAdminCommand, payload)
			if response.Code != http.StatusOK {
				t.Fatalf("admin status=%d body=%q", response.Code, response.Body.String())
			}
			var envelope protocol.Envelope
			var result core.Response
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(envelope.Payload, &result); err != nil || !result.Success || !mutated {
				t.Fatalf("admin result=%#v mutated=%t error=%v", result, mutated, err)
			}
			values, ok := result.Result.(map[string]any)
			if !ok || values["state"] != "done" {
				t.Fatalf("result=%#v", result.Result)
			}
		})
	}
}

func TestJobsAndServicesCanUseTypedRuntimeOperations(t *testing.T) {
	for _, workload := range []model.WorkloadType{model.WorkloadJob, model.WorkloadService} {
		t.Run(string(workload), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixtureForWorkload(t, store, workload)
			operations := &recordingOperations{result: map[string]any{"value": string(make([]byte, 4*1024))}}
			server := newCallbackTestServer(t, store, func(config *Config) { config.Operations = operations })
			response := runtimeControlCall(t, server, spec, "/v1/runtime/operation/execute", protocol.MessageAdminCommand, operationCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: "request-1",
				Operation: "example.inspect", Input: map[string]any{"id": "one"},
				User: execution.SystemUser(),
			})
			if response.Code != http.StatusOK || operations.operation != "example.inspect" || operations.input["id"] != "one" {
				t.Fatalf("status=%d body=%q operation=%#v", response.Code, response.Body.String(), operations)
			}
			if operations.caller.ExecutionID != "execution-1" || operations.caller.Workload != workload {
				t.Fatalf("runtime caller = %#v", operations.caller)
			}
			if response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
				t.Fatalf("content length=%q body bytes=%d", response.Header().Get("Content-Length"), response.Body.Len())
			}
		})
	}
}

func TestRuntimeOperationPreservesNullResults(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	server := newCallbackTestServer(t, store, func(config *Config) {
		config.Operations = &recordingOperations{result: nil}
	})
	response := runtimeControlCall(t, server, spec, "/v1/runtime/operation/execute", protocol.MessageAdminCommand, operationCallPayload{
		ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: "request-1",
		Operation: "example.optional", User: execution.SystemUser(),
	})
	var envelope protocol.Envelope
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &envelope) != nil {
		t.Fatalf("runtime operation response: status=%d", response.Code)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil || string(payload["success"]) != "true" || string(payload["result"]) != "null" {
		t.Fatalf("null must remain explicit on the JSON bridge: %s", envelope.Payload)
	}
}

func TestJobsAndServicesCanUseKernelOwnedDatabase(t *testing.T) {
	for _, workload := range []model.WorkloadType{model.WorkloadJob, model.WorkloadService} {
		t.Run(string(workload), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixtureForWorkload(t, store, workload)
			databaseService := &recordingDatabase{}
			server := newCallbackTestServer(t, store, func(config *Config) { config.Database = databaseService })
			payload := databaseCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: "request-database",
				Statement: "SELECT $1", Parameters: json.RawMessage(`[3]`), ReturnRows: true, ReturnInsertID: true,
			}
			response := runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, payload)
			if response.Code != http.StatusOK || len(databaseService.statements) != 1 || !databaseService.statements[0].ReturnRows || !databaseService.statements[0].ReturnInsertID || string(databaseService.statements[0].Parameters) != `[3]` {
				t.Fatalf("query status=%d body=%q database=%#v", response.Code, response.Body.String(), databaseService)
			}
			var envelope protocol.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.MessageType != protocol.MessageDatabaseResult {
				t.Fatalf("query envelope=%#v error=%v", envelope, err)
			}
			payload.Statement, payload.Parameters, payload.ReturnRows, payload.ReturnInsertID = "DELETE FROM example", nil, false, false
			response = runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, payload)
			if response.Code != http.StatusOK || len(databaseService.statements) != 2 || databaseService.statements[1].ReturnRows {
				t.Fatalf("execute status=%d body=%q database=%#v", response.Code, response.Body.String(), databaseService)
			}
			payload.RequestID = "next-execution-scope"
			response = runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, payload)
			if response.Code != http.StatusOK || len(databaseService.statements) != 3 {
				t.Fatalf("execution-scoped status=%d database=%#v", response.Code, databaseService)
			}
		})
	}
}

func TestConcurrentRuntimeDatabaseCallsDoNotSerializeTheCallbackServer(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	const calls = 16
	databaseService := &recordingDatabase{entered: make(chan struct{}, calls), release: make(chan struct{})}
	server := newCallbackTestServer(t, store, func(config *Config) { config.Database = databaseService })
	responses := make(chan *httptest.ResponseRecorder, calls)
	for index := range calls {
		go func() {
			responses <- runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, databaseCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: fmt.Sprintf("request-%d", index),
				Statement: "SELECT 1", ReturnRows: true,
			})
		}()
	}
	for index := range calls {
		select {
		case <-databaseService.entered:
		case <-time.After(time.Second):
			close(databaseService.release)
			t.Fatalf("only %d of %d calls entered the database concurrently", index, calls)
		}
	}
	close(databaseService.release)
	for range calls {
		if response := <-responses; response.Code != http.StatusOK {
			t.Fatalf("concurrent callback returned %d: %s", response.Code, response.Body.String())
		}
	}
}

func TestConcurrentRuntimeDatabaseReadLoad(t *testing.T) {
	root := t.TempDir()
	store, _ := state.New(filepath.Join(root, "groups"))
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 4,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := newCallbackTestServer(t, store, func(config *Config) { config.Database = db })

	const calls = 128
	start := make(chan struct{})
	errorsFound := make(chan error, calls)
	var wait sync.WaitGroup
	wait.Add(calls)
	for index := range calls {
		go func() {
			defer wait.Done()
			<-start
			response := runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, databaseCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: fmt.Sprintf("request-%d", index),
				Statement: "SELECT 1", ReturnRows: true,
			})
			if response.Code != http.StatusOK {
				errorsFound <- fmt.Errorf("call %d returned %d: %s", index, response.Code, response.Body.String())
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func TestDatabaseScopeCleanupUsesExactExecutionIdentity(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	requestID := "request-database"
	databaseService := &recordingDatabase{}
	server := newCallbackTestServer(t, store, func(config *Config) { config.Database = databaseService })
	payload := databaseCallPayload{
		ExecutionID: "execution-1", WorkerID: "worker-1", RequestID: requestID,
	}
	response := runtimeControlCall(t, server, spec, "/v1/runtime/database/scope", protocol.MessageDatabaseExecute, payload)
	if response.Code != http.StatusOK || len(databaseService.closed) != 1 {
		t.Fatalf("exact cleanup status=%d database=%#v", response.Code, databaseService)
	}
	wantedWorker := databaseScope(spec.RuntimeGroupID, spec.SandboxID, payload.WorkerID, payload.ExecutionID)
	if databaseService.closed[0] != wantedWorker+"\x00"+requestID {
		t.Fatalf("closed scope = %q", databaseService.closed[0])
	}
	payload.RequestID = ""
	response = runtimeControlCall(t, server, spec, "/v1/runtime/database/scope", protocol.MessageDatabaseExecute, payload)
	if response.Code != http.StatusOK || len(databaseService.prefixes) != 1 || databaseService.prefixes[0] != wantedWorker {
		t.Fatalf("Worker cleanup status=%d database=%#v", response.Code, databaseService)
	}
}

func TestActiveServiceCanInvokeOneExactWorker(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	requestID := "request-control"
	invoker := &recordingWorkerInvoker{result: nodes.WorkerInvocationResult{OK: true, Output: map[string]any{"state": "live"}}}
	server := newCallbackTestServer(t, store, func(config *Config) {
		config.WorkerInvoker = invoker
	})
	payload := workerCallPayload{
		ExecutionID: "source-execution", SourceWorkerID: "source-worker", RequestID: requestID,
		TargetNodeID: "node-b", TargetSandboxID: "sandbox-b", TargetWorkerID: "worker-b",
		TargetPersistentExecutionID: "persistent-target",
		Function:                    "example.inspect", Input: map[string]any{"id": "opaque"}, User: execution.SystemUser(),
	}
	response := runtimeControlCall(t, server, spec, "/v1/runtime/worker/invoke", protocol.MessageWorkerInvoke, payload)
	if response.Code != http.StatusOK {
		t.Fatalf("invoke status=%d body=%q", response.Code, response.Body.String())
	}
	if len(invoker.calls) != 1 || invoker.calls[0].NodeID != "node-b" || invoker.calls[0].SandboxID != "sandbox-b" || invoker.calls[0].WorkerID != "worker-b" || invoker.calls[0].PersistentExecutionID != "persistent-target" || invoker.calls[0].Function != "example.inspect" || invoker.calls[0].User != execution.SystemUser() {
		t.Fatalf("exact invocation=%#v", invoker.calls)
	}
	var envelope protocol.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.MessageType != protocol.MessageWorkerResult {
		t.Fatalf("response envelope=%#v err=%v", envelope, err)
	}
	var result nodes.WorkerInvocationResult
	if err := json.Unmarshal(envelope.Payload, &result); err != nil || !result.OK {
		t.Fatalf("result=%#v err=%v", result, err)
	}

}

func runtimeControlCall(t *testing.T, server *Server, spec model.SandboxSpec, path string, messageType protocol.MessageType, payload any) *httptest.ResponseRecorder {
	t.Helper()
	payloadData, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.Envelope{ProtocolVersion: protocol.ProtocolVersion, MessageType: messageType, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: "control-correlation", Payload: payloadData})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://callback"+path, bytes.NewReader(body))
	request.RemoteAddr = "10.88.0.2:1000"
	request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	response := httptest.NewRecorder()
	server.serveHTTP(response, request)
	return response
}

func callbackMessage(t testing.TB, messageType protocol.MessageType, spec model.SandboxSpec, payload statusPayload) []byte {
	t.Helper()
	if payload.Revision == 0 {
		payload.Revision = 1
	}
	payloadData, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(protocol.Envelope{ProtocolVersion: protocol.ProtocolVersion, MessageType: messageType, RuntimeGroupID: spec.RuntimeGroupID, Payload: payloadData})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCallbackListenerLifecycle(t *testing.T) {
	store, _ := state.New(t.TempDir())
	socketPath := filepath.Join(t.TempDir(), "kernel.sock")
	server, err := New(Config{Store: store, ProtocolVersion: protocol.ProtocolVersion, SocketPath: socketPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	if server.Address() != socketPath {
		t.Fatalf("callback address=%q", server.Address())
	}
	info, err := os.Stat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("socket info=%#v err=%v", info, err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after close: %v", err)
	}
}

func TestCallbackClientReconnectsAfterSocketReplacement(t *testing.T) {
	root := t.TempDir()
	store, _ := state.New(filepath.Join(root, "groups"))
	spec, status := callbackFixture(t, store)
	socketPath := filepath.Join(root, "runtime", "kernel.sock")
	newServer := func() *Server {
		server, err := New(Config{Store: store, ProtocolVersion: protocol.ProtocolVersion, SocketPath: socketPath})
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	revision := uint64(1)
	call := func() {
		body := callbackMessage(t, protocol.MessageHeartbeat, spec, statusPayload{
			Revision:        revision,
			ProtocolVersion: protocol.ProtocolVersion, RuntimeGroupID: spec.RuntimeGroupID,
			SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType),
		})
		request, err := http.NewRequest(http.MethodPost, "http://kernel/v1/runtime/heartbeat", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", response.StatusCode)
		}
		revision++
	}
	first := newServer()
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	call()
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	status.SupervisorHealthy = false
	if err := store.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		t.Fatal(err)
	}
	second := newServer()
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	call()
	_, observed, err := store.Load(spec.RuntimeGroupID)
	if err != nil || !observed.SupervisorHealthy {
		t.Fatalf("observed=%#v err=%v", observed, err)
	}
}

func callbackFixture(t testing.TB, store *state.Store) (model.SandboxSpec, model.SandboxStatus) {
	return callbackFixtureForWorkload(t, store, model.WorkloadJob)
}

func callbackFixtureForWorkload(t testing.TB, store *state.Store, workload model.WorkloadType) (model.SandboxSpec, model.SandboxStatus) {
	t.Helper()
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profile := model.RuntimeProfile{WorkloadType: workload, ImageDigest: digest, DependencyMode: model.DependencyCachedOnly, NetworkMode: "netstack", ResourceClass: string(workload)}
	hash, _ := profile.Hash()
	spec := model.SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: workload, GroupKey: string(workload) + ":owner:test", OwnerIDs: []string{"test"}, ImageDigest: digest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: model.ResourceLimits{PIDMaximum: 1, TmpfsMaximum: 1}, Network: model.NetworkConfiguration{Mode: "netstack", NetworkName: "the8020"}, DependencyMode: model.DependencyCachedOnly, Lifecycle: model.LifecyclePolicy{}, InternalToken: "0123456789abcdef0123456789abcdef"}
	status := model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateStarting}
	if err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		t.Fatal(err)
	}
	return spec, status
}
