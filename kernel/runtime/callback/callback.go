// Package callback receives authenticated supervisor registration and heartbeats.
package callback

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/database"
	"the8020/kernel/execution"
	"the8020/kernel/nodes"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

type Config struct {
	Store           *state.Store
	ProtocolVersion int
	SocketPath      string
	Now             func() time.Time
	Authentication  Authentication
	RuntimeRequests RuntimeRequests
	Database        Database
	AdminBus        AdminBus
	WorkerInvoker   WorkerInvoker
	Persistent      PersistentExecutionCompleter
	Operations      RuntimeOperations
}

type Authentication interface {
	Login(context.Context, string, string, bool) (auth.LoginResult, error)
	LogoutCurrentContext(context.Context, auth.AuthContext, bool) (auth.LogoutResult, error)
}

type RuntimeRequests interface {
	RuntimeRequest(string) (auth.RuntimeRequest, bool)
}

type AdminBus interface {
	Execute(context.Context, core.Request) core.Response
}

type Database interface {
	Status() database.Status
	Execute(context.Context, string, []any) (database.ExecuteResult, error)
	RunStatement(context.Context, string, database.StatementRequest) (database.StatementResult, error)
	BeginTransaction(context.Context, string, database.TransactionSettings) (string, error)
	FinishTransaction(context.Context, string, string, bool) error
	CloseScope(string)
	CloseScopePrefix(string)
}

type WorkerInvoker interface {
	InvokeWorker(context.Context, nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult
}

type PersistentExecutionCompleter interface {
	CompletePersistentExecution(context.Context, PersistentExecutionTarget) error
}

type PersistentExecutionTarget struct {
	RuntimeGroupID        string
	SandboxID             string
	WorkerID              string
	ServiceID             string
	PersistentExecutionID string
}

// RuntimeOperations exposes typed kernel primitives to trusted package code.
// It is deliberately separate from the public command registry.
type RuntimeOperations interface {
	Execute(context.Context, string, map[string]any) (any, error)
}

type Server struct {
	store           *state.Store
	protocolVersion int
	socketPath      string
	now             func() time.Time
	authentication  Authentication
	runtimeRequests RuntimeRequests
	adminBus        AdminBus
	database        Database
	workerInvoker   WorkerInvoker
	persistent      PersistentExecutionCompleter
	operations      RuntimeOperations
	mu              sync.Mutex
	listener        net.Listener
	httpServer      *http.Server
}

type statusPayload struct {
	Revision              uint64                      `json:"revision"`
	SupervisorStartedAtMS int64                       `json:"supervisor_started_at_ms"`
	ProtocolVersion       int                         `json:"protocol_version"`
	SupervisorVersion     string                      `json:"supervisor_version"`
	DenoVersion           string                      `json:"deno_version"`
	RuntimeGroupID        string                      `json:"runtime_group_id"`
	SandboxID             string                      `json:"sandbox_id"`
	WorkloadType          string                      `json:"workload_type"`
	WorkerCount           int                         `json:"worker_count"`
	ReadyWorkerCount      int                         `json:"ready_worker_count"`
	FailedWorkerCount     int                         `json:"failed_worker_count"`
	ActiveRequests        int                         `json:"active_requests"`
	ActiveExecutionCount  int                         `json:"active_execution_count"`
	UptimeMS              int64                       `json:"uptime_ms"`
	Draining              bool                        `json:"draining"`
	EventLoopTime         int64                       `json:"event_loop_timestamp,omitempty"`
	MemoryUsage           json.RawMessage             `json:"memory_usage,omitempty"`
	RecentFailures        []workerFailure             `json:"recent_failures,omitempty"`
	Workers               []model.RuntimeWorkerStatus `json:"workers"`
}

type workerFailure struct {
	WorkerID    string `json:"worker_id"`
	ExecutionID string `json:"execution_id"`
	Reason      string `json:"reason"`
}

type authCallPayload struct {
	ExecutionID string `json:"execution_id"`
	WorkerID    string `json:"worker_id"`
	RequestID   string `json:"request_id"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type adminCallPayload struct {
	ExecutionID string         `json:"execution_id"`
	WorkerID    string         `json:"worker_id"`
	RequestID   string         `json:"request_id"`
	CommandID   string         `json:"command_id"`
	Arguments   map[string]any `json:"arguments"`
}

type databaseCallPayload struct {
	ExecutionID    string          `json:"execution_id"`
	WorkerID       string          `json:"worker_id"`
	RequestID      string          `json:"request_id"`
	Statement      string          `json:"statement"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
	ReturnRows     bool            `json:"return_rows,omitempty"`
	ReturnInsertID bool            `json:"return_insert_id,omitempty"`
	Transaction    string          `json:"transaction,omitempty"`
	Operation      string          `json:"operation,omitempty"`
	Settings       json.RawMessage `json:"settings,omitempty"`
}

type workerCallPayload struct {
	ExecutionID                 string `json:"execution_id"`
	SourceWorkerID              string `json:"worker_id"`
	RequestID                   string `json:"request_id"`
	TargetNodeID                string `json:"target_node_id"`
	TargetSandboxID             string `json:"target_sandbox_id"`
	TargetWorkerID              string `json:"target_worker_id"`
	TargetPersistentExecutionID string `json:"target_persistent_execution_id"`
	Function                    string `json:"function"`
	Input                       any    `json:"input"`
}

type completionCallPayload struct {
	ExecutionID           string `json:"execution_id"`
	WorkerID              string `json:"worker_id"`
	ServiceID             string `json:"service_id"`
	RequestID             string `json:"request_id"`
	PersistentExecutionID string `json:"persistent_execution_id"`
}

type operationCallPayload struct {
	ExecutionID string         `json:"execution_id"`
	WorkerID    string         `json:"worker_id"`
	RequestID   string         `json:"request_id"`
	Operation   string         `json:"operation"`
	Input       map[string]any `json:"input"`
}

type operationCallResult struct {
	Success bool        `json:"success"`
	Result  any         `json:"result,omitempty"`
	Error   *core.Error `json:"error,omitempty"`
}

func New(config Config) (*Server, error) {
	if config.Store == nil || config.ProtocolVersion < 1 || !filepath.IsAbs(config.SocketPath) {
		return nil, errors.New("state store, protocol version, and absolute kernel socket path are required")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Server{store: config.Store, protocolVersion: config.ProtocolVersion, socketPath: filepath.Clean(config.SocketPath), now: config.Now, authentication: config.Authentication, runtimeRequests: config.RuntimeRequests, database: config.Database, adminBus: config.AdminBus, workerInvoker: config.WorkerInvoker, persistent: config.Persistent, operations: config.Operations}, nil
}

func (s *Server) SetWorkerInvoker(invoker WorkerInvoker) {
	s.mu.Lock()
	s.workerInvoker = invoker
	s.mu.Unlock()
}

func (s *Server) SetRuntimeOperations(operations RuntimeOperations) {
	s.mu.Lock()
	s.operations = operations
	s.mu.Unlock()
}

func (s *Server) SetAuthentication(authentication Authentication, requests RuntimeRequests) {
	s.mu.Lock()
	s.authentication = authentication
	s.runtimeRequests = requests
	s.mu.Unlock()
}

func (s *Server) SetPersistentExecutionCompleter(completer PersistentExecutionCompleter) {
	s.mu.Lock()
	s.persistent = completer
	s.mu.Unlock()
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return errors.New("runtime callback server is already started")
	}
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("create runtime callback socket directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("make runtime callback socket directory accessible: %w", err)
	}
	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("runtime callback socket path is occupied by a non-socket")
		}
		if connection, dialErr := net.DialTimeout("unix", s.socketPath, 100*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			return errors.New("runtime callback socket is already active")
		}
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("remove stale runtime callback socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime callback socket: %w", err)
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen for runtime callbacks: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o666); err != nil {
		_ = listener.Close()
		return fmt.Errorf("make runtime callback socket accessible: %w", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	s.listener, s.httpServer = listener, server
	go func() { _ = server.Serve(listener) }()
	return nil
}

func (s *Server) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.socketPath
}

func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	server, listener := s.httpServer, s.listener
	s.httpServer, s.listener = nil, nil
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	if listener != nil {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	if removeErr := os.Remove(s.socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func (s *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || !validCallbackPath(request.URL.Path) {
		http.NotFound(writer, request)
		return
	}
	var message protocol.Envelope
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		http.Error(writer, "invalid runtime envelope", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid runtime envelope", http.StatusBadRequest)
		return
	}
	wantType := callbackMessageType(request.URL.Path)
	if err := message.Validate(); err != nil || message.ProtocolVersion != s.protocolVersion || message.MessageType != wantType {
		http.Error(writer, "runtime protocol mismatch", http.StatusBadRequest)
		return
	}
	spec, status, ok := s.store.Cached(message.RuntimeGroupID)
	if !ok {
		http.Error(writer, "unknown runtime group", http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if len(token) != len(spec.InternalToken) || subtle.ConstantTimeCompare([]byte(token), []byte(spec.InternalToken)) != 1 {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status.ObservedState == model.StateFailed || status.ObservedState == model.StateStopping || status.ObservedState == model.StateStopped || status.ObservedState == model.StateDeleting {
		http.Error(writer, "runtime group is not accepting callbacks", http.StatusConflict)
		return
	}
	if wantType == protocol.MessageAuthLogin || wantType == protocol.MessageAuthLogoutCurrent {
		s.handleAuthentication(writer, request, message, spec)
		return
	}
	if wantType == protocol.MessageAdminCommand {
		if request.URL.Path == "/v1/runtime/operation/execute" {
			s.handleOperation(writer, request, message, spec)
		} else {
			s.handleAdministration(writer, request, message, spec)
		}
		return
	}
	if wantType == protocol.MessageDatabaseExecute {
		s.handleDatabase(writer, request, message, spec)
		return
	}
	if wantType == protocol.MessageWorkerInvoke {
		s.handleWorkerInvocation(writer, request, message, spec)
		return
	}
	if wantType == protocol.MessagePersistentExecutionComplete {
		s.handlePersistentExecutionCompletion(writer, request, message, spec)
		return
	}
	var payload statusPayload
	if err := decodePayload(message.Payload, &payload); err != nil {
		http.Error(writer, "invalid runtime payload", http.StatusBadRequest)
		return
	}
	if payload.ProtocolVersion != s.protocolVersion || payload.RuntimeGroupID != message.RuntimeGroupID {
		http.Error(writer, "runtime protocol mismatch", http.StatusBadRequest)
		return
	}
	if payload.SandboxID != spec.SandboxID || payload.WorkloadType != string(spec.WorkloadType) {
		http.Error(writer, "runtime identity mismatch", http.StatusBadRequest)
		return
	}
	_, err := s.store.Observe(spec.RuntimeGroupID, model.RuntimeSnapshot{
		Revision: payload.Revision, SupervisorStartedAtMS: payload.SupervisorStartedAtMS, ProtocolVersion: payload.ProtocolVersion,
		SupervisorVersion: payload.SupervisorVersion, DenoVersion: payload.DenoVersion,
		RuntimeGroupID: payload.RuntimeGroupID, SandboxID: payload.SandboxID,
		WorkloadType: model.WorkloadType(payload.WorkloadType), WorkerCount: payload.WorkerCount,
		ReadyWorkerCount: payload.ReadyWorkerCount, FailedWorkerCount: payload.FailedWorkerCount,
		ActiveRequests: payload.ActiveRequests, ActiveExecutionCount: payload.ActiveExecutionCount,
		UptimeMS: payload.UptimeMS, Draining: payload.Draining,
		RecentFailures: runtimeFailures(payload.RecentFailures), Workers: payload.Workers,
	}, s.now())
	if err != nil {
		http.Error(writer, "record runtime snapshot", http.StatusInternalServerError)
		return
	}
	writeJSON(writer, map[string]bool{"accepted": true})
}

func runtimeFailures(values []workerFailure) []model.RuntimeFailure {
	result := make([]model.RuntimeFailure, len(values))
	for index, value := range values {
		result[index] = model.RuntimeFailure{WorkerID: value.WorkerID, ExecutionID: value.ExecutionID, Reason: value.Reason}
	}
	return result
}

func validCallbackPath(path string) bool {
	switch path {
	case "/v1/runtime/register", "/v1/runtime/heartbeat", "/v1/runtime/auth/login", "/v1/runtime/auth/logout-current", "/v1/runtime/admin/execute", "/v1/runtime/operation/execute", "/v1/runtime/database/info", "/v1/runtime/database/execute", "/v1/runtime/database/transaction", "/v1/runtime/database/scope", "/v1/runtime/worker/invoke", "/v1/runtime/execution/complete":
		return true
	default:
		return false
	}
}

func callbackMessageType(path string) protocol.MessageType {
	switch path {
	case "/v1/runtime/register":
		return protocol.MessageSupervisorRegistration
	case "/v1/runtime/auth/login":
		return protocol.MessageAuthLogin
	case "/v1/runtime/auth/logout-current":
		return protocol.MessageAuthLogoutCurrent
	case "/v1/runtime/admin/execute", "/v1/runtime/operation/execute":
		return protocol.MessageAdminCommand
	case "/v1/runtime/database/info", "/v1/runtime/database/execute", "/v1/runtime/database/transaction", "/v1/runtime/database/scope":
		return protocol.MessageDatabaseExecute
	case "/v1/runtime/worker/invoke":
		return protocol.MessageWorkerInvoke
	case "/v1/runtime/execution/complete":
		return protocol.MessagePersistentExecutionComplete
	default:
		return protocol.MessageHeartbeat
	}
}

func (s *Server) handleDatabase(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	if s.database == nil {
		http.Error(writer, "system database is unavailable", http.StatusServiceUnavailable)
		return
	}
	if (spec.WorkloadType != model.WorkloadService && spec.WorkloadType != model.WorkloadJob) || message.CorrelationID == "" {
		http.Error(writer, "runtime database identity mismatch", http.StatusBadRequest)
		return
	}
	var payload databaseCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.ExecutionID == "" || payload.WorkerID == "" {
		http.Error(writer, "invalid runtime database payload", http.StatusBadRequest)
		return
	}
	if request.URL.Path == "/v1/runtime/database/info" {
		s.writeDatabaseResult(writer, message, spec, s.database.Status())
		return
	}
	workerScope := databaseScope(spec.RuntimeGroupID, spec.SandboxID, payload.WorkerID, payload.ExecutionID)
	if request.URL.Path == "/v1/runtime/database/scope" && payload.RequestID == "" {
		s.database.CloseScopePrefix(workerScope)
		s.writeDatabaseResult(writer, message, spec, map[string]any{"closed": true})
		return
	}
	if payload.RequestID == "" {
		http.Error(writer, "runtime database execution context is required", http.StatusConflict)
		return
	}
	scope := workerScope + "\x00" + payload.RequestID
	if request.URL.Path == "/v1/runtime/database/scope" {
		s.database.CloseScope(scope)
		s.writeDatabaseResult(writer, message, spec, map[string]any{"closed": true})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	var result any
	var err error
	switch request.URL.Path {
	case "/v1/runtime/database/execute":
		result, err = s.database.RunStatement(ctx, scope, database.StatementRequest{
			Statement: payload.Statement, Parameters: payload.Parameters, ReturnRows: payload.ReturnRows,
			ReturnInsertID: payload.ReturnInsertID, Transaction: payload.Transaction,
		})
	case "/v1/runtime/database/transaction":
		switch payload.Operation {
		case "database.transaction.begin":
			var settings database.TransactionSettings
			if len(payload.Settings) != 0 {
				err = json.Unmarshal(payload.Settings, &settings)
			}
			if err == nil {
				var token string
				token, err = s.database.BeginTransaction(ctx, scope, settings)
				result = map[string]any{"transaction": token}
			}
		case "database.transaction.commit", "database.transaction.rollback":
			err = s.database.FinishTransaction(ctx, scope, payload.Transaction, payload.Operation == "database.transaction.commit")
			result = map[string]any{"completed": err == nil}
		default:
			err = errors.New("unknown database transaction operation")
		}
	}
	if err != nil {
		http.Error(writer, "database operation failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.writeDatabaseResult(writer, message, spec, result)
}

func databaseScope(runtimeGroupID, sandboxID, workerID, executionID string) string {
	return strings.Join([]string{runtimeGroupID, sandboxID, workerID, executionID}, "\x00")
}

func (s *Server) writeDatabaseResult(writer http.ResponseWriter, message protocol.Envelope, spec model.SandboxSpec, result any) {
	data, err := json.Marshal(result)
	limit := 2 << 20
	if s.database != nil && s.database.Status().MaximumResultBytes > limit {
		limit = s.database.Status().MaximumResultBytes + 1<<20
	}
	if err != nil || len(data) > limit {
		http.Error(writer, "encode database result", http.StatusInternalServerError)
		return
	}
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageDatabaseResult, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: data}
	writeJSON(writer, response)
}

func (s *Server) handleWorkerInvocation(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	s.mu.Lock()
	invoker := s.workerInvoker
	s.mu.Unlock()
	if invoker == nil {
		http.Error(writer, "runtime Worker control is unavailable", http.StatusServiceUnavailable)
		return
	}
	if (spec.WorkloadType != model.WorkloadService && spec.WorkloadType != model.WorkloadJob) || message.CorrelationID == "" {
		http.Error(writer, "runtime Worker control identity mismatch", http.StatusBadRequest)
		return
	}
	var payload workerCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil {
		http.Error(writer, "invalid Worker invocation payload", http.StatusBadRequest)
		return
	}
	if payload.ExecutionID == "" || payload.SourceWorkerID == "" || payload.RequestID == "" {
		http.Error(writer, "runtime Worker control request is not active", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	result := invoker.InvokeWorker(ctx, nodes.WorkerInvocationRequest{
		NodeID: payload.TargetNodeID, SandboxID: payload.TargetSandboxID,
		WorkerID: payload.TargetWorkerID, PersistentExecutionID: payload.TargetPersistentExecutionID,
		Function: payload.Function, Input: payload.Input,
	})
	data, err := json.Marshal(result)
	if err != nil || len(data) > 1<<20 {
		http.Error(writer, "encode Worker invocation result", http.StatusInternalServerError)
		return
	}
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageWorkerResult, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: data}
	writeJSON(writer, response)
}

func (s *Server) handlePersistentExecutionCompletion(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	s.mu.Lock()
	completer := s.persistent
	s.mu.Unlock()
	if completer == nil || spec.WorkloadType != model.WorkloadService || message.CorrelationID == "" {
		http.Error(writer, "persistent execution completion is unavailable", http.StatusServiceUnavailable)
		return
	}
	var payload completionCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.ExecutionID == "" || payload.WorkerID == "" || payload.ServiceID == "" || payload.RequestID == "" || payload.PersistentExecutionID == "" {
		http.Error(writer, "invalid persistent execution completion", http.StatusBadRequest)
		return
	}
	err := completer.CompletePersistentExecution(request.Context(), PersistentExecutionTarget{
		RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID,
		WorkerID: payload.WorkerID, ServiceID: payload.ServiceID,
		PersistentExecutionID: payload.PersistentExecutionID,
	})
	if err != nil {
		http.Error(writer, "persistent execution target mismatch", http.StatusConflict)
		return
	}
	data := json.RawMessage(`{"completed":true}`)
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessagePersistentExecutionCompleted, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: data}
	writeJSON(writer, response)
}

func decodePayload(data json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing runtime payload data")
	}
	return nil
}

func (s *Server) handleAuthentication(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	if s.authentication == nil || s.runtimeRequests == nil {
		http.Error(writer, "runtime authentication is unavailable", http.StatusServiceUnavailable)
		return
	}
	if spec.WorkloadType != model.WorkloadService || message.CorrelationID == "" {
		http.Error(writer, "runtime authentication identity mismatch", http.StatusBadRequest)
		return
	}
	var payload authCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil {
		http.Error(writer, "invalid runtime authentication payload", http.StatusBadRequest)
		return
	}
	active, exists := s.runtimeRequests.RuntimeRequest(payload.RequestID)
	if !exists || payload.ExecutionID == "" || payload.WorkerID == "" || active.RuntimeGroupID != spec.RuntimeGroupID || active.SandboxID != spec.SandboxID {
		http.Error(writer, "runtime authentication request is not active", http.StatusConflict)
		return
	}
	var result any
	if message.MessageType == protocol.MessageAuthLogin {
		login, err := s.authentication.Login(request.Context(), payload.Username, payload.Password, active.SecureTransport)
		result = login
		if err != nil && login.Error == "" {
			http.Error(writer, "runtime authentication failed", http.StatusInternalServerError)
			return
		}
	} else {
		logout, err := s.authentication.LogoutCurrentContext(request.Context(), active.Auth, active.SecureTransport)
		if err != nil {
			http.Error(writer, "runtime authentication failed", http.StatusInternalServerError)
			return
		}
		result = logout
	}
	payloadData, err := json.Marshal(result)
	if err != nil {
		http.Error(writer, "encode runtime authentication result", http.StatusInternalServerError)
		return
	}
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageAuthResult, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: payloadData}
	writeJSON(writer, response)
}

func (s *Server) handleAdministration(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	if s.adminBus == nil {
		http.Error(writer, "runtime administration is unavailable", http.StatusServiceUnavailable)
		return
	}
	if (spec.WorkloadType != model.WorkloadService && spec.WorkloadType != model.WorkloadJob) || message.CorrelationID == "" {
		http.Error(writer, "runtime administration identity mismatch", http.StatusBadRequest)
		return
	}
	var payload adminCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.CommandID == "" {
		http.Error(writer, "invalid runtime administration payload", http.StatusBadRequest)
		return
	}
	if payload.ExecutionID == "" || payload.WorkerID == "" || payload.RequestID == "" {
		http.Error(writer, "runtime administration request is not active", http.StatusConflict)
		return
	}
	callContext := execution.WithCaller(request.Context(), execution.Caller{ExecutionID: payload.ExecutionID, Workload: spec.WorkloadType})
	result := s.adminBus.Execute(callContext, core.Request{
		ProtocolVersion: core.ProtocolVersion,
		CommandID:       payload.CommandID,
		Arguments:       payload.Arguments,
		RequestID:       message.CorrelationID,
	})
	payloadData, err := json.Marshal(result)
	if err != nil {
		http.Error(writer, "encode runtime administration result", http.StatusInternalServerError)
		return
	}
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageAdminResult, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: payloadData}
	writeJSON(writer, response)
}

func (s *Server) handleOperation(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	s.mu.Lock()
	operations := s.operations
	s.mu.Unlock()
	if operations == nil {
		http.Error(writer, "runtime operations are unavailable", http.StatusServiceUnavailable)
		return
	}
	if (spec.WorkloadType != model.WorkloadService && spec.WorkloadType != model.WorkloadJob) || message.CorrelationID == "" {
		http.Error(writer, "runtime operation identity mismatch", http.StatusBadRequest)
		return
	}
	var payload operationCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.Operation == "" || payload.ExecutionID == "" || payload.WorkerID == "" || payload.RequestID == "" {
		http.Error(writer, "invalid runtime operation payload", http.StatusBadRequest)
		return
	}
	callContext := execution.WithCaller(request.Context(), execution.Caller{ExecutionID: payload.ExecutionID, Workload: spec.WorkloadType})
	result, operationErr := operations.Execute(callContext, payload.Operation, payload.Input)
	responseResult := operationCallResult{Success: operationErr == nil, Result: result}
	if operationErr != nil {
		var commandError *core.Error
		if errors.As(operationErr, &commandError) {
			responseResult.Error = commandError
		} else {
			responseResult.Error = core.NewError(core.CodeRuntimeOperation, operationErr.Error())
		}
		responseResult.Result = nil
	}
	payloadData, err := json.Marshal(responseResult)
	if err != nil || len(payloadData) > 2<<20 {
		http.Error(writer, "encode runtime operation result", http.StatusInternalServerError)
		return
	}
	response := protocol.Envelope{ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageAdminResult, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: message.CorrelationID, Payload: payloadData}
	writeJSON(writer, response)
}

func writeJSON(writer http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, "encode runtime response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = writer.Write(data)
}
