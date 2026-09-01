// Package supervisor communicates with the Deno supervisor in each runtime group.
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
)

type Config struct {
	ProtocolVersion int
	Port            int
	HTTPClient      *http.Client
	Endpoint        func(model.SandboxSpec) (string, error)
}

type Client struct {
	protocolVersion int
	httpClient      *http.Client
	endpoint        func(model.SandboxSpec) (string, error)
}

// ResponseError preserves the status of a rejected supervisor request so
// callers can distinguish a stable request/entrypoint rejection from a
// transient transport failure without parsing error text.
type ResponseError struct {
	Method     string
	Path       string
	Status     string
	StatusCode int
	Message    string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("supervisor %s %s returned %s: %s", e.Method, e.Path, e.Status, e.Message)
}

func IsRequestRejected(err error) bool {
	var response *ResponseError
	return errors.As(err, &response) && response.StatusCode >= 400 && response.StatusCode < 500
}

type Status struct {
	ProtocolVersion      int                `json:"protocol_version"`
	SupervisorVersion    string             `json:"supervisor_version"`
	DenoVersion          string             `json:"deno_version"`
	RuntimeGroupID       string             `json:"runtime_group_id"`
	SandboxID            string             `json:"sandbox_id"`
	WorkloadType         model.WorkloadType `json:"workload_type"`
	WorkerCount          int                `json:"worker_count"`
	ReadyWorkerCount     int                `json:"ready_worker_count"`
	FailedWorkerCount    int                `json:"failed_worker_count"`
	ActiveRequests       int                `json:"active_requests"`
	ActiveExecutionCount int                `json:"active_execution_count"`
	UptimeMS             int64              `json:"uptime_ms"`
	Draining             bool               `json:"draining"`
	RecentFailures       []WorkerFailure    `json:"recent_failures,omitempty"`
}

type WorkerFailure struct {
	WorkerID    string `json:"worker_id"`
	ExecutionID string `json:"execution_id"`
	Reason      string `json:"reason"`
}

type WorkerStatus struct {
	WorkerID     string     `json:"worker_id"`
	ExecutionID  string     `json:"execution_id"`
	WorkloadID   string     `json:"workload_id"`
	OwnerID      string     `json:"owner_id"`
	DebuggerName string     `json:"debugger_name"`
	Entrypoint   string     `json:"entrypoint"`
	ReleaseID    string     `json:"release_id"`
	InFlight     int        `json:"in_flight"`
	IdleSinceMS  int64      `json:"idle_since_ms,omitempty"`
	State        string     `json:"state"`
	Failure      string     `json:"failure,omitempty"`
	Logs         []LogEvent `json:"logs,omitempty"`
}

type ExecutionMetadata struct {
	WorkerID           string                    `json:"workerId"`
	ExecutionID        string                    `json:"executionId"`
	WorkloadType       model.WorkloadType        `json:"workloadType"`
	OwnerID            string                    `json:"ownerId"`
	WorkloadID         string                    `json:"workloadId"`
	ReleaseID          string                    `json:"releaseId"`
	Entrypoint         string                    `json:"entrypoint"`
	DebuggerName       string                    `json:"debuggerName"`
	ValidateEntrypoint bool                      `json:"validateEntrypoint,omitempty"`
	Service            *ServiceExecutionMetadata `json:"service,omitempty"`
}

type ServiceExecutionMetadata struct {
	ServiceID         string          `json:"serviceId"`
	Generation        uint64          `json:"generation"`
	CanonicalBasePath string          `json:"canonicalBasePath"`
	OpenAPI           OpenAPIMetadata `json:"openapi,omitempty"`
	ExecutionMode     string          `json:"executionMode,omitempty"`
}

type WorkerInvocationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type WorkerInvocationResult struct {
	OK     bool                   `json:"ok"`
	Output any                    `json:"output,omitempty"`
	Error  *WorkerInvocationError `json:"error,omitempty"`
}

type OpenAPIMetadata struct {
	Title       string `json:"title,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

type WorkerPermissions struct {
	Read   []string `json:"read,omitempty"`
	Write  []string `json:"write,omitempty"`
	Net    []string `json:"net,omitempty"`
	Import []string `json:"import,omitempty"`
	Env    []string `json:"env,omitempty"`
	Sys    []string `json:"sys,omitempty"`
}

type LogEvent struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type JobResult struct {
	Result any        `json:"result"`
	Logs   []LogEvent `json:"logs,omitempty"`
}

type StartWorkerRequest struct {
	Metadata    ExecutionMetadata `json:"metadata"`
	Permissions WorkerPermissions `json:"permissions"`
}

func New(config Config) (*Client, error) {
	if config.ProtocolVersion < 1 {
		return nil, errors.New("runtime protocol version is required")
	}
	if config.Port == 0 {
		config.Port = 8000
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("supervisor port must be between 1 and 65535")
	}
	if config.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		config.HTTPClient = &http.Client{Transport: transport}
	}
	if config.Endpoint == nil {
		port := config.Port
		config.Endpoint = func(spec model.SandboxSpec) (string, error) {
			if spec.Network.SupervisorPort != 0 {
				return EndpointForSandbox(spec, spec.Network.SupervisorPort)
			}
			return EndpointForSandbox(spec, port)
		}
	}
	return &Client{protocolVersion: config.ProtocolVersion, httpClient: config.HTTPClient, endpoint: config.Endpoint}, nil
}

func EndpointForSandbox(spec model.SandboxSpec, port int) (string, error) {
	if net.ParseIP(spec.Network.SandboxIP) == nil || port < 1 || port > 65535 {
		return "", errors.New("sandbox IP and valid supervisor port are required")
	}
	return "http://" + net.JoinHostPort(spec.Network.SandboxIP, strconv.Itoa(port)), nil
}

func (c *Client) Status(ctx context.Context, spec model.SandboxSpec) (Status, error) {
	var status Status
	if err := c.query(ctx, spec, "/v1/status", &status); err != nil {
		return status, err
	}
	if status.ProtocolVersion != c.protocolVersion {
		return Status{}, fmt.Errorf("supervisor protocol version %d does not match %d", status.ProtocolVersion, c.protocolVersion)
	}
	if status.RuntimeGroupID != spec.RuntimeGroupID || status.SandboxID != spec.SandboxID || status.WorkloadType != spec.WorkloadType {
		return Status{}, errors.New("supervisor identity does not match sandbox specification")
	}
	return status, nil
}

func (c *Client) Workers(ctx context.Context, spec model.SandboxSpec) ([]WorkerStatus, error) {
	var response struct {
		Workers []WorkerStatus `json:"workers"`
	}
	if err := c.query(ctx, spec, "/v1/workers", &response); err != nil {
		return nil, err
	}
	return response.Workers, nil
}

func (c *Client) StartWorker(ctx context.Context, spec model.SandboxSpec, request StartWorkerRequest) (WorkerStatus, error) {
	if request.Metadata.WorkloadType != spec.WorkloadType || request.Metadata.WorkerID == "" || request.Metadata.ExecutionID == "" {
		return WorkerStatus{}, errors.New("Worker identity and matching workload type are required")
	}
	var response struct {
		Worker WorkerStatus `json:"worker"`
	}
	if err := c.control(ctx, spec, "/v1/workers/start", protocol.MessageStartWorker, request, protocol.MessageWorkerStateChange, &response); err != nil {
		return WorkerStatus{}, err
	}
	return response.Worker, nil
}

func (c *Client) StopWorker(ctx context.Context, spec model.SandboxSpec, workerID string, immediate bool) error {
	path := "/v1/workers/" + url.PathEscape(workerID) + "/stop"
	if immediate {
		path += "?immediate=true"
	}
	return c.control(ctx, spec, path, protocol.MessageStopWorker, struct {
		Immediate bool `json:"immediate"`
	}{Immediate: immediate}, protocol.MessageWorkerStateChange, nil)
}

func (c *Client) InvokeWorker(ctx context.Context, spec model.SandboxSpec, workerID, function string, input any) (WorkerInvocationResult, error) {
	if workerID == "" || function == "" || len(function) > 128 {
		return WorkerInvocationResult{}, errors.New("Worker ID and registered function are required")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return WorkerInvocationResult{}, errors.New("Worker invocation input must be JSON serializable")
	}
	if len(payload) > 1<<20 {
		return WorkerInvocationResult{}, errors.New("Worker invocation input exceeds 1 MiB")
	}
	var result WorkerInvocationResult
	if err := c.control(ctx, spec, "/v1/workers/"+url.PathEscape(workerID)+"/invoke", protocol.MessageWorkerInvoke, struct {
		Function string `json:"function"`
		Input    any    `json:"input"`
	}{Function: function, Input: input}, protocol.MessageWorkerResult, &result); err != nil {
		return WorkerInvocationResult{}, err
	}
	encoded, err := json.Marshal(result.Output)
	if err != nil || len(encoded) > 1<<20 {
		return WorkerInvocationResult{}, errors.New("Worker invocation output is invalid or exceeds 1 MiB")
	}
	if result.OK == (result.Error != nil) {
		return WorkerInvocationResult{}, errors.New("Worker invocation result is malformed")
	}
	return result, nil
}

func (c *Client) RunJob(ctx context.Context, spec model.SandboxSpec, workerID string, input any) (JobResult, error) {
	var response JobResult
	if err := c.control(ctx, spec, "/v1/jobs/"+url.PathEscape(workerID)+"/run", protocol.MessageJobStart, struct {
		Input any `json:"input"`
	}{Input: input}, protocol.MessageJobResult, &response); err != nil {
		return JobResult{}, err
	}
	return response, nil
}

func (c *Client) ConfigureService(ctx context.Context, spec model.SandboxSpec, serviceID string, workerIDs []string, concurrencyPerWorker int) error {
	body := struct {
		WorkerIDs            []string `json:"worker_ids"`
		ConcurrencyPerWorker int      `json:"concurrency_per_worker"`
	}{WorkerIDs: append([]string{}, workerIDs...), ConcurrencyPerWorker: concurrencyPerWorker}
	return c.control(ctx, spec, "/v1/services/"+url.PathEscape(serviceID)+"/configure", protocol.MessageServicePoolConfiguration, body, protocol.MessageServicePoolConfiguration, nil)
}

func (c *Client) ServiceOpenAPI(ctx context.Context, spec model.SandboxSpec, serviceID string) (map[string]any, error) {
	var response struct {
		Document map[string]any `json:"document"`
	}
	if err := c.control(ctx, spec, "/v1/services/"+url.PathEscape(serviceID)+"/openapi", protocol.MessageServiceOpenapi, struct{}{}, protocol.MessageServiceOpenapi, &response); err != nil {
		return nil, err
	}
	if response.Document == nil {
		return nil, errors.New("supervisor returned an empty OpenAPI document")
	}
	return response.Document, nil
}

func (c *Client) Drain(ctx context.Context, spec model.SandboxSpec) error {
	return c.control(ctx, spec, "/v1/drain", protocol.MessageRuntimeDrain, struct{}{}, protocol.MessageRuntimeDrain, nil)
}

func (c *Client) DispatchService(ctx context.Context, spec model.SandboxSpec, serviceID string, original *http.Request) (*http.Response, error) {
	endpoint, err := c.endpoint(spec)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/services/"+url.PathEscape(serviceID)+"/dispatch", original.Body)
	if err != nil {
		return nil, err
	}
	request.Header = original.Header.Clone()
	request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	request.Header.Set("X-80-20-Method", original.Method)
	request.Header.Set("X-80-20-URL", original.URL.String())
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if (response.StatusCode < 200 || response.StatusCode >= 300) && response.Header.Get("X-80-20-Service-Response") != "true" {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return nil, fmt.Errorf("supervisor service dispatch returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	response.Header.Del("X-80-20-Service-Response")
	return response, nil
}

// ProxyServiceWebSocket preserves the upgraded byte stream while replacing the
// public hop with the authenticated supervisor hop selected by the kernel.
func (c *Client) ProxyServiceWebSocket(ctx context.Context, spec model.SandboxSpec, serviceID string, writer http.ResponseWriter, original *http.Request) error {
	if len(spec.InternalToken) < 16 {
		return errors.New("sandbox internal token is unavailable")
	}
	endpoint, err := c.endpoint(spec)
	if err != nil {
		return err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	director := func(request *http.Request) {
		request.Header.Set("X-80-20-URL", original.URL.String())
		request.URL.Scheme = target.Scheme
		request.URL.Host = target.Host
		request.URL.Path = "/v1/services/" + url.PathEscape(serviceID) + "/websocket"
		request.URL.RawPath = ""
		request.Host = target.Host
		request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	}
	proxy := &httputil.ReverseProxy{Director: director, Transport: c.httpClient.Transport}
	proxy.ServeHTTP(writer, original.WithContext(ctx))
	return nil
}

func (c *Client) query(ctx context.Context, spec model.SandboxSpec, path string, output any) error {
	if len(spec.InternalToken) < 16 {
		return errors.New("sandbox internal token is unavailable")
	}
	endpoint, err := c.endpoint(spec)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return &ResponseError{Method: http.MethodGet, Path: path, Status: response.Status, StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode supervisor response: %w", err)
	}
	return nil
}

func (c *Client) control(ctx context.Context, spec model.SandboxSpec, path string, messageType protocol.MessageType, input any, responseType protocol.MessageType, output any) error {
	if len(spec.InternalToken) < 16 {
		return errors.New("sandbox internal token is unavailable")
	}
	endpoint, err := c.endpoint(spec)
	if err != nil {
		return err
	}
	correlationID, err := model.NewID("correlation")
	if err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	data, err := json.Marshal(protocol.Envelope{ProtocolVersion: c.protocolVersion, MessageType: messageType, RuntimeGroupID: spec.RuntimeGroupID, CorrelationID: correlationID, Payload: payload})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return &ResponseError{Method: http.MethodPost, Path: path, Status: response.Status, StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	var envelope protocol.Envelope
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode supervisor envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode supervisor envelope: trailing data")
	}
	if err := envelope.Validate(); err != nil {
		return err
	}
	if envelope.ProtocolVersion != c.protocolVersion || envelope.MessageType != responseType || envelope.RuntimeGroupID != spec.RuntimeGroupID || envelope.CorrelationID != correlationID {
		return errors.New("supervisor response envelope does not match request")
	}
	if output == nil {
		return nil
	}
	payloadDecoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	payloadDecoder.UseNumber()
	if err := payloadDecoder.Decode(output); err != nil {
		return fmt.Errorf("decode supervisor response payload: %w", err)
	}
	return nil
}
