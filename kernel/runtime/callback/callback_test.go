package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	platformauth "the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/execution"
	"the8020/kernel/nodes"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

type fixedRuntimeRequests struct {
	requests map[string]platformauth.RuntimeRequest
}

func (r fixedRuntimeRequests) RuntimeRequest(id string) (platformauth.RuntimeRequest, bool) {
	request, ok := r.requests[id]
	return request, ok
}

type recordingWorkerInvoker struct {
	calls  []nodes.WorkerInvocationRequest
	result nodes.WorkerInvocationResult
}

func (i *recordingWorkerInvoker) InvokeWorker(_ context.Context, input nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult {
	i.calls = append(i.calls, input)
	return i.result
}

type recordingPersistentCompleter struct {
	calls []PersistentExecutionTarget
	err   error
}

type runtimeIdentityCall struct {
	runtimeGroupID string
	sandboxID      string
	workerID       string
	executionID    string
	workloadID     string
}

type recordingRuntimeIdentity struct {
	calls []runtimeIdentityCall
	err   error
}

func (v *recordingRuntimeIdentity) ValidateRuntimeExecution(_ context.Context, runtimeGroupID, sandboxID, workerID, executionID, workloadID string) error {
	v.calls = append(v.calls, runtimeIdentityCall{runtimeGroupID: runtimeGroupID, sandboxID: sandboxID, workerID: workerID, executionID: executionID, workloadID: workloadID})
	return v.err
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

func newCallbackTestServer(t *testing.T, store *state.Store, configure func(*Config)) *Server {
	t.Helper()
	config := Config{
		Store: store, ProtocolVersion: protocol.ProtocolVersion,
		SocketPath: filepath.Join(t.TempDir(), "kernel.sock"),
		Workers:    &recordingRuntimeIdentity{},
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
	queries    []string
	executions []string
	parameters []any
	statements []database.StatementRequest
	closed     []string
	prefixes   []string
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

func (c *recordingPersistentCompleter) CompletePersistentExecution(_ context.Context, target PersistentExecutionTarget) error {
	c.calls = append(c.calls, target)
	return c.err
}

func TestAuthenticatedRegistrationAndHeartbeatPersistStatus(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, status := callbackFixture(t, store)
	now := time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC)
	server := newCallbackTestServer(t, store, func(config *Config) { config.Now = func() time.Time { return now } })
	for _, endpoint := range []struct{ path, message string }{{"/v1/runtime/register", "supervisor_registration"}, {"/v1/runtime/heartbeat", "heartbeat"}} {
		body := callbackMessage(t, protocol.MessageType(endpoint.message), spec, statusPayload{ProtocolVersion: 1, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType), SupervisorVersion: "1.0.0", DenoVersion: "2.9.4", WorkerCount: 2})
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
}

func TestCallbackRejectsTokenAndProtocolMismatch(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixture(t, store)
	server := newCallbackTestServer(t, store, nil)
	validPayload := statusPayload{ProtocolVersion: 1, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType)}
	for _, test := range []struct {
		name, remote, token string
		version             int
		want                int
	}{
		{"token", "10.88.0.2:1", "wrong", 1, http.StatusUnauthorized},
		{"protocol", "10.88.0.2:1", spec.InternalToken, 2, http.StatusBadRequest},
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
	body := callbackMessage(t, protocol.MessageHeartbeat, spec, statusPayload{ProtocolVersion: 1, RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, WorkloadType: string(spec.WorkloadType)})
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

func TestSupervisorMediatedAuthenticationCallsRequireActiveRequestIdentity(t *testing.T) {
	root := t.TempDir()
	store, _ := state.New(filepath.Join(root, "groups"))
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	manager := newCallbackTestAuthentication(t, root)
	runtimeIdentity := &recordingRuntimeIdentity{}
	server := newCallbackTestServer(t, store, func(config *Config) {
		config.Authentication, config.RuntimeRequests = manager, manager
		config.Workers = runtimeIdentity
	})
	release, err := manager.BeginRuntimeRequest(platformauth.RuntimeRequest{RequestID: "request-1", ServiceID: "example/auth/login", RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, SecureTransport: true})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	call := func(path string, messageType protocol.MessageType, payload authCallPayload) *httptest.ResponseRecorder {
		payloadData, _ := json.Marshal(payload)
		body, _ := json.Marshal(protocol.Envelope{ProtocolVersion: 1, MessageType: messageType, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: "correlation-1", Payload: payloadData})
		request := httptest.NewRequest(http.MethodPost, "http://callback"+path, bytes.NewReader(body))
		request.RemoteAddr = "10.88.0.2:1000"
		request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
		response := httptest.NewRecorder()
		server.serveHTTP(response, request)
		return response
	}

	identity := authCallPayload{ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "service-version-login", ServiceID: "example/auth/login", RequestID: "request-1", SandboxID: spec.SandboxID}
	mismatched := identity
	mismatched.RequestID = "request-other"
	if response := call("/v1/runtime/auth/login", protocol.MessageAuthLogin, mismatched); response.Code != http.StatusConflict {
		t.Fatalf("mismatched active request status=%d body=%q", response.Code, response.Body.String())
	}
	identity.Username, identity.Password = "admin", "password"
	loginResponse := call("/v1/runtime/auth/login", protocol.MessageAuthLogin, identity)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%q", loginResponse.Code, loginResponse.Body.String())
	}
	var envelope protocol.Envelope
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &envelope); err != nil || envelope.MessageType != protocol.MessageAuthResult || envelope.CorrelationID != "correlation-1" {
		t.Fatalf("login envelope=%#v error=%v", envelope, err)
	}
	var login platformauth.LoginResult
	if err := json.Unmarshal(envelope.Payload, &login); err != nil || !login.Authenticated || login.SetCookie == "" {
		t.Fatalf("login=%#v error=%v", login, err)
	}
	if len(runtimeIdentity.calls) != 1 || runtimeIdentity.calls[0].workloadID != "service-version-login" {
		t.Fatalf("runtime identity=%#v", runtimeIdentity.calls)
	}
	cookieResponse := &http.Response{Header: http.Header{"Set-Cookie": []string{login.SetCookie}}}
	cookies := cookieResponse.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("secure transport cookie=%#v", cookies)
	}
	authContext, err := manager.ValidateCookie(cookies[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	release()
	releaseLogout, err := manager.BeginRuntimeRequest(platformauth.RuntimeRequest{RequestID: "request-2", ServiceID: "example/auth/logout", RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, Auth: authContext, SecureTransport: true})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLogout()
	logoutIdentity := authCallPayload{ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "service-version-login", ServiceID: "example/auth/logout", RequestID: "request-2", SandboxID: spec.SandboxID}
	logoutResponse := call("/v1/runtime/auth/logout-current", protocol.MessageAuthLogoutCurrent, logoutIdentity)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%q", logoutResponse.Code, logoutResponse.Body.String())
	}
	if _, err := manager.ValidateCookie(cookies[0].Value); err == nil {
		t.Fatal("logout callback did not revoke current authentication session")
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
			identity := &recordingRuntimeIdentity{}
			server := newCallbackTestServer(t, store, func(config *Config) {
				config.AdminBus, config.Workers = registry, identity
			})
			payload := adminCallPayload{
				ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "service-version-control", ServiceID: "example/control", SandboxID: spec.SandboxID,
				CommandID: "kernel.test", Arguments: map[string]any{"value": "accepted"},
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
			if !ok || values["state"] != "done" || len(identity.calls) != 1 || identity.calls[0].workloadID != "service-version-control" {
				t.Fatalf("result=%#v identity=%#v", result.Result, identity.calls)
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
				ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "the8020/example/workload",
				ServiceID: "the8020/example/service", SandboxID: spec.SandboxID, Operation: "example.inspect", Input: map[string]any{"id": "one"},
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

func newCallbackTestAuthentication(t *testing.T, root string) *platformauth.Manager {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS "the8020__users__users" ("username" TEXT PRIMARY KEY, "passwordHash" TEXT NOT NULL, "enabled" INTEGER NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE IF NOT EXISTS "the8020__users__sessions" ("sessionId" TEXT PRIMARY KEY, "username" TEXT NOT NULL, "secretHash" TEXT NOT NULL, "authVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "expiresAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	parameters := platformauth.Argon2Parameters{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, OutputLength: 16}
	hasher, err := platformauth.NewPasswordHasher(parameters, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := platformauth.New(platformauth.Config{
		Database: db, SessionDuration: time.Hour, CleanupInterval: time.Hour,
		Argon2: parameters, Hasher: hasher,
	})
	if err != nil {
		t.Fatal(err)
	}
	passwordHash, err := hasher.Hash("password")
	if err != nil {
		t.Fatal(err)
	}
	now := database.EncodeTime(db, time.Now())
	if _, err := db.ExecContext(context.Background(), `INSERT INTO "the8020__users__users" ("username", "passwordHash", "enabled", "authVersion", "createdAt", "updatedAt") VALUES ($1, $2, $3, 1, $4, $4)`, "admin", passwordHash, true, now); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestActiveServiceRequestCanUseKernelOwnedDatabase(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	active := platformauth.RuntimeRequest{
		RequestID: "request-database", ServiceID: "example/data/service",
		RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID,
	}
	databaseService := &recordingDatabase{}
	server := newCallbackTestServer(t, store, func(config *Config) { config.Database = databaseService })
	payload := databaseCallPayload{
		ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "service-version-data", ServiceID: active.ServiceID,
		RequestID: active.RequestID, SandboxID: spec.SandboxID,
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
	payload.RequestID = "job-or-request-scope"
	response = runtimeControlCall(t, server, spec, "/v1/runtime/database/execute", protocol.MessageDatabaseExecute, payload)
	if response.Code != http.StatusOK || len(databaseService.statements) != 3 {
		t.Fatalf("execution-scoped status=%d database=%#v", response.Code, databaseService)
	}
}

func TestDatabaseScopeCleanupUsesExactExecutionIdentity(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	active := platformauth.RuntimeRequest{
		RequestID: "request-database", ServiceID: "example/data/service",
		RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID,
	}
	databaseService := &recordingDatabase{}
	server := newCallbackTestServer(t, store, func(config *Config) { config.Database = databaseService })
	payload := databaseCallPayload{
		ExecutionID: "execution-1", WorkerID: "worker-1", WorkloadID: "service-version-data", ServiceID: active.ServiceID,
		RequestID: active.RequestID, SandboxID: spec.SandboxID,
	}
	response := runtimeControlCall(t, server, spec, "/v1/runtime/database/scope", protocol.MessageDatabaseExecute, payload)
	if response.Code != http.StatusOK || len(databaseService.closed) != 1 {
		t.Fatalf("exact cleanup status=%d database=%#v", response.Code, databaseService)
	}
	wantedWorker := databaseScope(spec.RuntimeGroupID, spec.SandboxID, payload.WorkerID, payload.ExecutionID)
	if databaseService.closed[0] != wantedWorker+"\x00"+active.ServiceID+"\x00"+active.RequestID {
		t.Fatalf("closed scope = %q", databaseService.closed[0])
	}
	payload.RequestID, payload.ServiceID = "", ""
	response = runtimeControlCall(t, server, spec, "/v1/runtime/database/scope", protocol.MessageDatabaseExecute, payload)
	if response.Code != http.StatusOK || len(databaseService.prefixes) != 1 || databaseService.prefixes[0] != wantedWorker {
		t.Fatalf("Worker cleanup status=%d database=%#v", response.Code, databaseService)
	}
}

func TestActiveServiceCanInvokeOneExactWorker(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	active := platformauth.RuntimeRequest{
		RequestID: "request-control", ServiceID: "example/admin/control",
		RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID,
	}
	invoker := &recordingWorkerInvoker{result: nodes.WorkerInvocationResult{OK: true, Output: map[string]any{"state": "live"}}}
	identity := &recordingRuntimeIdentity{}
	server := newCallbackTestServer(t, store, func(config *Config) {
		config.WorkerInvoker, config.Workers = invoker, identity
	})
	payload := workerCallPayload{
		ExecutionID: "source-execution", SourceWorkerID: "source-worker", WorkloadID: "service-version-control", ServiceID: active.ServiceID,
		RequestID: active.RequestID, SourceSandboxID: spec.SandboxID,
		TargetNodeID: "node-b", TargetSandboxID: "sandbox-b", TargetWorkerID: "worker-b",
		TargetPersistentExecutionID: "persistent-target",
		Function:                    "example.inspect", Input: map[string]any{"id": "opaque"},
	}
	response := runtimeControlCall(t, server, spec, "/v1/runtime/worker/invoke", protocol.MessageWorkerInvoke, payload)
	if response.Code != http.StatusOK {
		t.Fatalf("invoke status=%d body=%q", response.Code, response.Body.String())
	}
	if len(invoker.calls) != 1 || invoker.calls[0].NodeID != "node-b" || invoker.calls[0].SandboxID != "sandbox-b" || invoker.calls[0].WorkerID != "worker-b" || invoker.calls[0].PersistentExecutionID != "persistent-target" || invoker.calls[0].Function != "example.inspect" {
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

	identity.err = errors.New("stale execution")
	response = runtimeControlCall(t, server, spec, "/v1/runtime/worker/invoke", protocol.MessageWorkerInvoke, payload)
	if response.Code != http.StatusConflict || len(invoker.calls) != 1 {
		t.Fatalf("inactive execution status=%d calls=%#v", response.Code, invoker.calls)
	}
}

func TestPersistentCompletionCarriesExactGenericExecutionIdentity(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, model.WorkloadService)
	completer := &recordingPersistentCompleter{}
	server := newCallbackTestServer(t, store, func(config *Config) { config.Persistent = completer })
	payload := completionCallPayload{
		ExecutionID: "worker-execution", WorkerID: "worker-a", WorkloadID: "service-version-realtime", ServiceID: "example/realtime/channel",
		RequestID: "request-old", SandboxID: spec.SandboxID, PersistentExecutionID: "persistent-a",
	}
	response := runtimeControlCall(t, server, spec, "/v1/runtime/execution/complete", protocol.MessagePersistentExecutionComplete, payload)
	if response.Code != http.StatusOK || len(completer.calls) != 1 {
		t.Fatalf("completion status=%d calls=%#v body=%q", response.Code, completer.calls, response.Body.String())
	}
	target := completer.calls[0]
	if target.RuntimeGroupID != spec.RuntimeGroupID || target.SandboxID != spec.SandboxID || target.WorkerID != "worker-a" || target.ServiceID != "example/realtime/channel" || target.PersistentExecutionID != "persistent-a" {
		t.Fatalf("completion target=%#v", target)
	}

	payload.SandboxID = "sandbox-other"
	response = runtimeControlCall(t, server, spec, "/v1/runtime/execution/complete", protocol.MessagePersistentExecutionComplete, payload)
	if response.Code != http.StatusBadRequest || len(completer.calls) != 1 {
		t.Fatalf("mismatch status=%d calls=%#v", response.Code, completer.calls)
	}
	payload.SandboxID = spec.SandboxID
	completer.err = context.Canceled
	response = runtimeControlCall(t, server, spec, "/v1/runtime/execution/complete", protocol.MessagePersistentExecutionComplete, payload)
	if response.Code != http.StatusConflict || len(completer.calls) != 2 {
		t.Fatalf("rejected completion status=%d calls=%#v", response.Code, completer.calls)
	}
}

func runtimeControlCall(t *testing.T, server *Server, spec model.SandboxSpec, path string, messageType protocol.MessageType, payload any) *httptest.ResponseRecorder {
	t.Helper()
	payloadData, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.Envelope{ProtocolVersion: 1, MessageType: messageType, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: "control-correlation", Payload: payloadData})
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

func callbackMessage(t *testing.T, messageType protocol.MessageType, spec model.SandboxSpec, payload statusPayload) []byte {
	t.Helper()
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
	server, err := New(Config{Store: store, ProtocolVersion: protocol.ProtocolVersion, SocketPath: socketPath, Workers: &recordingRuntimeIdentity{}})
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
		server, err := New(Config{Store: store, ProtocolVersion: protocol.ProtocolVersion, SocketPath: socketPath, Workers: &recordingRuntimeIdentity{}})
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
	call := func() {
		body := callbackMessage(t, protocol.MessageHeartbeat, spec, statusPayload{
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

func callbackFixture(t *testing.T, store *state.Store) (model.SandboxSpec, model.SandboxStatus) {
	return callbackFixtureForWorkload(t, store, model.WorkloadJob)
}

func callbackFixtureForWorkload(t *testing.T, store *state.Store, workload model.WorkloadType) (model.SandboxSpec, model.SandboxStatus) {
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
