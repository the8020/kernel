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
	"the8020/kernel/nodes"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

type Config struct {
	Store            *state.Store
	ProtocolVersion  int
	BindAddress      string
	AdvertiseAddress string
	AllowedNetwork   *net.IPNet
	EndpointState    string
	Now              func() time.Time
	Authentication   Authentication
	RuntimeRequests  RuntimeRequests
	Database         Database
	AdminBus         AdminBus
	WorkerInvoker    WorkerInvoker
	Persistent       PersistentExecutionCompleter
}

type Authentication interface {
	BootstrapLogin(context.Context, string, string, bool) (auth.BootstrapLoginResult, error)
	LogoutCurrent(auth.AuthContext, bool) (auth.LogoutResult, error)
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

type Server struct {
	store            *state.Store
	protocolVersion  int
	bindAddress      string
	advertiseAddress string
	allowedNetwork   *net.IPNet
	endpointState    string
	now              func() time.Time
	authentication   Authentication
	runtimeRequests  RuntimeRequests
	adminBus         AdminBus
	database         Database
	workerInvoker    WorkerInvoker
	persistent       PersistentExecutionCompleter
	mu               sync.Mutex
	listener         net.Listener
	httpServer       *http.Server
}

var errTerminalRuntimeGroup = errors.New("runtime group is not accepting callbacks")

type statusPayload struct {
	ProtocolVersion      int             `json:"protocol_version"`
	SupervisorVersion    string          `json:"supervisor_version"`
	DenoVersion          string          `json:"deno_version"`
	RuntimeGroupID       string          `json:"runtime_group_id"`
	SandboxID            string          `json:"sandbox_id"`
	WorkloadType         string          `json:"workload_type"`
	WorkerCount          int             `json:"worker_count"`
	ReadyWorkerCount     int             `json:"ready_worker_count"`
	FailedWorkerCount    int             `json:"failed_worker_count"`
	ActiveRequests       int             `json:"active_requests"`
	ActiveExecutionCount int             `json:"active_execution_count"`
	UptimeMS             int64           `json:"uptime_ms"`
	Draining             bool            `json:"draining"`
	EventLoopTime        int64           `json:"event_loop_timestamp,omitempty"`
	MemoryUsage          json.RawMessage `json:"memory_usage,omitempty"`
	RecentFailures       []workerFailure `json:"recent_failures,omitempty"`
}

type workerFailure struct {
	WorkerID    string `json:"worker_id"`
	ExecutionID string `json:"execution_id"`
	Reason      string `json:"reason"`
}

type authCallPayload struct {
	ExecutionID string `json:"execution_id"`
	WorkerID    string `json:"worker_id"`
	ServiceID   string `json:"service_id"`
	RequestID   string `json:"request_id"`
	SandboxID   string `json:"sandbox_id"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type adminCallPayload struct {
	ExecutionID string         `json:"execution_id"`
	WorkerID    string         `json:"worker_id"`
	ServiceID   string         `json:"service_id"`
	RequestID   string         `json:"request_id"`
	SandboxID   string         `json:"sandbox_id"`
	CommandID   string         `json:"command_id"`
	Arguments   map[string]any `json:"arguments"`
}

type databaseCallPayload struct {
	ExecutionID string          `json:"execution_id"`
	WorkerID    string          `json:"worker_id"`
	ServiceID   string          `json:"service_id"`
	RequestID   string          `json:"request_id"`
	SandboxID   string          `json:"sandbox_id"`
	Statement   string          `json:"statement"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	ReturnRows  bool            `json:"return_rows,omitempty"`
	Transaction string          `json:"transaction,omitempty"`
	Operation   string          `json:"operation,omitempty"`
	Settings    json.RawMessage `json:"settings,omitempty"`
}

type workerCallPayload struct {
	ExecutionID                 string `json:"execution_id"`
	SourceWorkerID              string `json:"worker_id"`
	ServiceID                   string `json:"service_id"`
	RequestID                   string `json:"request_id"`
	SourceSandboxID             string `json:"sandbox_id"`
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
	SandboxID             string `json:"sandbox_id"`
	PersistentExecutionID string `json:"persistent_execution_id"`
}

func New(config Config) (*Server, error) {
	if config.Store == nil || config.ProtocolVersion < 1 || config.AdvertiseAddress == "" || config.AllowedNetwork == nil {
		return nil, errors.New("state store, protocol version, advertised address, and sandbox network are required")
	}
	if net.ParseIP(config.AdvertiseAddress) == nil {
		return nil, errors.New("callback advertise address must be an IP")
	}
	if config.BindAddress == "" {
		config.BindAddress = "0.0.0.0"
	}
	if net.ParseIP(config.BindAddress) == nil {
		return nil, errors.New("callback bind address must be an IP")
	}
	if config.EndpointState != "" && !filepath.IsAbs(config.EndpointState) {
		return nil, errors.New("runtime callback endpoint state path must be absolute")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Server{store: config.Store, protocolVersion: config.ProtocolVersion, bindAddress: config.BindAddress, advertiseAddress: config.AdvertiseAddress, allowedNetwork: config.AllowedNetwork, endpointState: config.EndpointState, now: config.Now, authentication: config.Authentication, runtimeRequests: config.RuntimeRequests, database: config.Database, adminBus: config.AdminBus, workerInvoker: config.WorkerInvoker, persistent: config.Persistent}, nil
}

func (s *Server) SetWorkerInvoker(invoker WorkerInvoker) {
	s.mu.Lock()
	s.workerInvoker = invoker
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
	port, err := loadEndpointPort(s.endpointState)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(s.bindAddress, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen for runtime callbacks: %w", err)
	}
	if port == 0 && s.endpointState != "" {
		port = listener.Addr().(*net.TCPAddr).Port
		if err := saveEndpointPort(s.endpointState, port); err != nil {
			_ = listener.Close()
			return err
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	s.listener, s.httpServer = listener, server
	go func() { _ = server.Serve(listener) }()
	return nil
}

type endpointRecord struct {
	Port int `json:"port"`
}

func loadEndpointPort(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read runtime callback endpoint: %w", err)
	}
	var record endpointRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return 0, fmt.Errorf("decode runtime callback endpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, errors.New("decode runtime callback endpoint: trailing data")
	}
	if record.Port < 1 || record.Port > 65535 {
		return 0, errors.New("runtime callback endpoint has an invalid port")
	}
	return record.Port, nil
}

func saveEndpointPort(path string, port int) error {
	if port < 1 || port > 65535 {
		return errors.New("runtime callback endpoint has an invalid port")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runtime callback state directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("restrict runtime callback state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".callback-*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime callback endpoint state: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	data, err := json.MarshalIndent(endpointRecord{Port: port}, "", "  ")
	if err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write runtime callback endpoint state: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace runtime callback endpoint state: %w", err)
	}
	return nil
}

func (s *Server) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return "http://" + net.JoinHostPort(s.advertiseAddress, strconv.Itoa(s.listener.Addr().(*net.TCPAddr).Port))
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
		err = errors.Join(err, listener.Close())
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || !validCallbackPath(request.URL.Path) {
		http.NotFound(writer, request)
		return
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || !s.allowedNetwork.Contains(net.ParseIP(host)) {
		http.Error(writer, "forbidden", http.StatusForbidden)
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
	spec, status, err := s.store.Load(message.RuntimeGroupID)
	if err != nil {
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
	if wantType == protocol.MessageAuthBootstrapLogin || wantType == protocol.MessageAuthLogoutCurrent {
		s.handleAuthentication(writer, request, message, spec)
		return
	}
	if wantType == protocol.MessageAdminCommand {
		s.handleAdministration(writer, request, message, spec)
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
	_, err = s.store.UpdateStatus(spec.RuntimeGroupID, func(current *model.SandboxStatus) error {
		if current.ObservedState == model.StateFailed || current.ObservedState == model.StateStopping || current.ObservedState == model.StateStopped || current.ObservedState == model.StateDeleting {
			return errTerminalRuntimeGroup
		}
		current.SupervisorHealthy = true
		current.SupervisorVersion = payload.SupervisorVersion
		current.DenoVersion = payload.DenoVersion
		current.WorkerCount = payload.WorkerCount
		current.LastHeartbeat = s.now()
		return nil
	})
	if err != nil {
		if errors.Is(err, errTerminalRuntimeGroup) {
			http.Error(writer, errTerminalRuntimeGroup.Error(), http.StatusConflict)
			return
		}
		http.Error(writer, "persist runtime heartbeat", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(`{"accepted":true}`))
}

func validCallbackPath(path string) bool {
	switch path {
	case "/v1/runtime/register", "/v1/runtime/heartbeat", "/v1/runtime/auth/bootstrap-login", "/v1/runtime/auth/logout-current", "/v1/runtime/admin/execute", "/v1/runtime/database/info", "/v1/runtime/database/execute", "/v1/runtime/database/transaction", "/v1/runtime/database/scope", "/v1/runtime/worker/invoke", "/v1/runtime/execution/complete":
		return true
	default:
		return false
	}
}

func callbackMessageType(path string) protocol.MessageType {
	switch path {
	case "/v1/runtime/register":
		return protocol.MessageSupervisorRegistration
	case "/v1/runtime/auth/bootstrap-login":
		return protocol.MessageAuthBootstrapLogin
	case "/v1/runtime/auth/logout-current":
		return protocol.MessageAuthLogoutCurrent
	case "/v1/runtime/admin/execute":
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
	if s.database == nil || s.runtimeRequests == nil {
		http.Error(writer, "system database is unavailable", http.StatusServiceUnavailable)
		return
	}
	if (spec.WorkloadType != model.WorkloadService && spec.WorkloadType != model.WorkloadJob) || message.CorrelationID == "" {
		http.Error(writer, "runtime database identity mismatch", http.StatusBadRequest)
		return
	}
	var payload databaseCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.ExecutionID == "" || payload.WorkerID == "" || payload.SandboxID != spec.SandboxID {
		http.Error(writer, "invalid runtime database payload", http.StatusBadRequest)
		return
	}
	if request.URL.Path == "/v1/runtime/database/info" {
		s.writeDatabaseResult(writer, message, spec, s.database.Status())
		return
	}
	workerScope := databaseScope(spec.RuntimeGroupID, spec.SandboxID, payload.WorkerID, payload.ExecutionID)
	if request.URL.Path == "/v1/runtime/database/scope" && payload.RequestID == "" && payload.ServiceID == "" {
		s.database.CloseScopePrefix(workerScope)
		s.writeDatabaseResult(writer, message, spec, map[string]any{"closed": true})
		return
	}
	if payload.RequestID == "" || payload.ServiceID == "" {
		http.Error(writer, "runtime database execution context is required", http.StatusConflict)
		return
	}
	if spec.WorkloadType == model.WorkloadService {
		active, exists := s.runtimeRequests.RuntimeRequest(payload.RequestID)
		if !exists || payload.ServiceID != active.ServiceID || active.RuntimeGroupID != spec.RuntimeGroupID || active.SandboxID != spec.SandboxID {
			http.Error(writer, "runtime database request is not active", http.StatusConflict)
			return
		}
	}
	scope := workerScope + "\x00" + payload.ServiceID + "\x00" + payload.RequestID
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
			Statement: payload.Statement, Parameters: payload.Parameters, ReturnRows: payload.ReturnRows, Transaction: payload.Transaction,
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
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}

func (s *Server) handleWorkerInvocation(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	s.mu.Lock()
	invoker := s.workerInvoker
	s.mu.Unlock()
	if invoker == nil || s.runtimeRequests == nil {
		http.Error(writer, "runtime Worker control is unavailable", http.StatusServiceUnavailable)
		return
	}
	if spec.WorkloadType != model.WorkloadService || message.CorrelationID == "" {
		http.Error(writer, "runtime Worker control identity mismatch", http.StatusBadRequest)
		return
	}
	var payload workerCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil {
		http.Error(writer, "invalid Worker invocation payload", http.StatusBadRequest)
		return
	}
	active, exists := s.runtimeRequests.RuntimeRequest(payload.RequestID)
	if !exists || payload.ExecutionID == "" || payload.SourceWorkerID == "" || payload.ServiceID != active.ServiceID || payload.SourceSandboxID != spec.SandboxID || active.RuntimeGroupID != spec.RuntimeGroupID || active.SandboxID != spec.SandboxID {
		http.Error(writer, "runtime Worker control request is not active", http.StatusConflict)
		return
	}
	if !active.Auth.Authenticated || active.Auth.UserID == "" {
		http.Error(writer, "authenticated administrator is required", http.StatusForbidden)
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
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
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
	if err := decodePayload(message.Payload, &payload); err != nil || payload.ExecutionID == "" || payload.WorkerID == "" || payload.ServiceID == "" || payload.SandboxID != spec.SandboxID || payload.PersistentExecutionID == "" {
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
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
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
	if !exists || payload.ExecutionID == "" || payload.WorkerID == "" || payload.ServiceID != active.ServiceID || payload.SandboxID != spec.SandboxID || active.RuntimeGroupID != spec.RuntimeGroupID || active.SandboxID != spec.SandboxID {
		http.Error(writer, "runtime authentication request is not active", http.StatusConflict)
		return
	}
	var result any
	if message.MessageType == protocol.MessageAuthBootstrapLogin {
		login, err := s.authentication.BootstrapLogin(request.Context(), payload.Username, payload.Password, active.SecureTransport)
		result = login
		if err != nil && login.Error == "" {
			http.Error(writer, "runtime authentication failed", http.StatusInternalServerError)
			return
		}
	} else {
		logout, err := s.authentication.LogoutCurrent(active.Auth, active.SecureTransport)
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
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		return
	}
}

func (s *Server) handleAdministration(writer http.ResponseWriter, request *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	if s.adminBus == nil || s.runtimeRequests == nil {
		http.Error(writer, "runtime administration is unavailable", http.StatusServiceUnavailable)
		return
	}
	if spec.WorkloadType != model.WorkloadService || message.CorrelationID == "" {
		http.Error(writer, "runtime administration identity mismatch", http.StatusBadRequest)
		return
	}
	var payload adminCallPayload
	if err := decodePayload(message.Payload, &payload); err != nil || payload.CommandID == "" {
		http.Error(writer, "invalid runtime administration payload", http.StatusBadRequest)
		return
	}
	active, exists := s.runtimeRequests.RuntimeRequest(payload.RequestID)
	if !exists || payload.ExecutionID == "" || payload.WorkerID == "" || payload.ServiceID != active.ServiceID || payload.SandboxID != spec.SandboxID || active.RuntimeGroupID != spec.RuntimeGroupID || active.SandboxID != spec.SandboxID {
		http.Error(writer, "runtime administration request is not active", http.StatusConflict)
		return
	}
	if !active.Auth.Authenticated || active.Auth.Realm != "bootstrap-admin" || active.Auth.UserID == "" {
		http.Error(writer, "bootstrap administrator is required", http.StatusForbidden)
		return
	}
	result := s.adminBus.Execute(request.Context(), core.Request{
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
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}
