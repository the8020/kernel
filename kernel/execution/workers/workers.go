// Package workers owns generic Worker validation, lookup, and supervisor delegation.
package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/nodes"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type Sandboxes interface {
	List() ([]manager.Inspection, error)
	Inspect(context.Context, string) (manager.Inspection, error)
	ResolveRuntimeGroup(string) (model.SandboxSpec, error)
}

type Control interface {
	Workers(context.Context, model.SandboxSpec) ([]supervisor.WorkerStatus, error)
	StartWorker(context.Context, model.SandboxSpec, supervisor.StartWorkerRequest) (supervisor.WorkerStatus, error)
	StopWorker(context.Context, model.SandboxSpec, string, bool) error
	InvokeWorker(context.Context, model.SandboxSpec, string, string, string, any) (supervisor.WorkerInvocationResult, error)
	RunJob(context.Context, model.SandboxSpec, string, []any, map[string]string, []string) (supervisor.JobResult, error)
	ConfigureService(context.Context, model.SandboxSpec, string, []string, int) error
	ServiceOpenAPI(context.Context, model.SandboxSpec, string) (map[string]any, error)
	DispatchService(context.Context, model.SandboxSpec, string, *http.Request) (*http.Response, error)
	ProxyServiceWebSocket(context.Context, model.SandboxSpec, string, http.ResponseWriter, *http.Request) error
}

type Manager struct {
	sandboxes                Sandboxes
	control                  Control
	maximumWorkers           int
	maximumWorkersPerSandbox int
	databaseBackend          string
	nodes                    interface {
		LocalNodeID() string
		InvokeWorker(context.Context, nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult
	}
	startMu sync.Mutex
}

func (m *Manager) SetNodeRouter(router interface {
	LocalNodeID() string
	InvokeWorker(context.Context, nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult
}) {
	m.nodes = router
}

type Record struct {
	SandboxID      string                  `json:"sandbox_id"`
	RuntimeGroupID string                  `json:"runtime_group_id"`
	WorkloadType   model.WorkloadType      `json:"workload_type"`
	Worker         supervisor.WorkerStatus `json:"worker"`
}

var (
	ErrNodeCapacity       = errors.New("node Worker capacity is exhausted")
	ErrSandboxCapacity    = errors.New("sandbox Worker capacity is exhausted")
	ErrRuntimeUnavailable = errors.New("runtime group is unavailable")
)

func New(sandboxes Sandboxes, control Control, maximumWorkers, maximumWorkersPerSandbox int, databaseBackend string) (*Manager, error) {
	if sandboxes == nil || control == nil {
		return nil, errors.New("sandbox catalog and supervisor control are required")
	}
	if maximumWorkers < 0 {
		return nil, errors.New("maximum node Worker count cannot be negative")
	}
	if maximumWorkersPerSandbox < 1 {
		return nil, errors.New("sandbox maximum Workers must be positive")
	}
	if databaseBackend != "sqlite" && databaseBackend != "postgresql" {
		return nil, errors.New("supported database backend is required")
	}
	return &Manager{sandboxes: sandboxes, control: control, maximumWorkers: maximumWorkers, maximumWorkersPerSandbox: maximumWorkersPerSandbox, databaseBackend: databaseBackend}, nil
}

func (m *Manager) Start(ctx context.Context, runtimeGroupID string, request supervisor.StartWorkerRequest) (Record, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.maximumWorkers > 0 {
		live, err := m.List(ctx, "")
		if err != nil {
			return Record{}, err
		}
		if len(live) >= m.maximumWorkers {
			return Record{}, fmt.Errorf("%w: %d of %d Workers are running", ErrNodeCapacity, len(live), m.maximumWorkers)
		}
	}
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return Record{}, err
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return Record{}, err
	}
	live, err := m.control.Workers(ctx, inspection.Spec)
	if err != nil {
		return Record{}, err
	}
	if len(live) >= m.maximumWorkersPerSandbox {
		return Record{}, fmt.Errorf("%w: %d of %d Workers are running", ErrSandboxCapacity, len(live), m.maximumWorkersPerSandbox)
	}
	if request.Metadata.WorkloadType != inspection.Spec.WorkloadType {
		return Record{}, errors.New("Worker workload type does not match runtime group")
	}
	if request.Metadata.DebuggerName == "" {
		request.Metadata.DebuggerName = fmt.Sprintf("%s:%s:%s:%s", request.Metadata.WorkloadType, request.Metadata.OwnerID, request.Metadata.ExecutionID, request.Metadata.WorkerID)
	}
	request.Metadata.DatabaseBackend = m.databaseBackend
	if err := validateEntrypoint(inspection.Spec, request.Metadata.Entrypoint); err != nil {
		return Record{}, err
	}
	if err := validatePermissions(inspection.Spec.Permissions, request.Permissions); err != nil {
		return Record{}, err
	}
	worker, err := m.control.StartWorker(ctx, inspection.Spec, request)
	if err != nil {
		return Record{}, err
	}
	return Record{SandboxID: inspection.Spec.SandboxID, RuntimeGroupID: inspection.Spec.RuntimeGroupID, WorkloadType: inspection.Spec.WorkloadType, Worker: worker}, nil
}

func (m *Manager) List(ctx context.Context, sandboxID string) ([]Record, error) {
	if sandboxID != "" {
		item, err := m.sandboxes.Inspect(ctx, sandboxID)
		if err != nil {
			return nil, err
		}
		if err := requireWorkerRuntime(item); err != nil {
			return nil, err
		}
		live, err := m.control.Workers(ctx, item.Spec)
		if err != nil {
			return nil, err
		}
		result := make([]Record, 0, len(live))
		for _, worker := range live {
			result = append(result, Record{SandboxID: item.Spec.SandboxID, RuntimeGroupID: item.Spec.RuntimeGroupID, WorkloadType: item.Spec.WorkloadType, Worker: worker})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Worker.WorkerID < result[j].Worker.WorkerID })
		return result, nil
	}
	items, err := m.sandboxes.List()
	if err != nil {
		return nil, err
	}
	var result []Record
	for _, item := range items {
		if requireWorkerRuntime(item) != nil {
			continue
		}
		live, workerErr := m.control.Workers(ctx, item.Spec)
		if workerErr != nil {
			return nil, workerErr
		}
		for _, worker := range live {
			result = append(result, Record{SandboxID: item.Spec.SandboxID, RuntimeGroupID: item.Spec.RuntimeGroupID, WorkloadType: item.Spec.WorkloadType, Worker: worker})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Worker.WorkerID < result[j].Worker.WorkerID })
	return result, nil
}

func (m *Manager) Inspect(ctx context.Context, workerID string) (Record, error) {
	records, err := m.List(ctx, "")
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.Worker.WorkerID == workerID {
			return record, nil
		}
	}
	return Record{}, fmt.Errorf("Worker %q not found", workerID)
}

// ValidateRuntimeExecution proves that identity supplied by the authenticated
// sandbox callback still names the exact live Worker execution.
func (m *Manager) ValidateRuntimeExecution(ctx context.Context, runtimeGroupID, sandboxID, workerID, executionID, workloadID string) error {
	if runtimeGroupID == "" || sandboxID == "" || workerID == "" || executionID == "" {
		return errors.New("runtime Worker execution identity is incomplete")
	}
	spec, err := m.sandboxes.ResolveRuntimeGroup(runtimeGroupID)
	if err != nil {
		return err
	}
	if spec.RuntimeGroupID != runtimeGroupID || spec.SandboxID != sandboxID {
		return errors.New("runtime Worker execution identity does not match")
	}
	workers, err := m.control.Workers(ctx, spec)
	if err != nil {
		return err
	}
	for _, worker := range workers {
		if worker.WorkerID != workerID {
			continue
		}
		if worker.ExecutionID != executionID {
			return errors.New("runtime Worker execution identity does not match")
		}
		if workloadID != "" && worker.WorkloadID != workloadID {
			return errors.New("runtime Worker workload identity does not match")
		}
		if worker.State != "ready" && worker.State != "draining" {
			return errors.New("runtime Worker is not active")
		}
		return nil
	}
	return fmt.Errorf("Worker %q not found in runtime group %q", workerID, runtimeGroupID)
}

func (m *Manager) Stop(ctx context.Context, workerID string, immediate bool) error {
	record, err := m.Inspect(ctx, workerID)
	if err != nil {
		return err
	}
	return m.StopInGroup(ctx, record.RuntimeGroupID, workerID, immediate)
}

// StopInGroup stops a Worker through its known runtime group without scanning
// unrelated sandboxes. Workload managers should use this path because their
// durable records already own the Worker-to-group association.
func (m *Manager) StopInGroup(ctx context.Context, runtimeGroupID, workerID string, immediate bool) error {
	if runtimeGroupID == "" || workerID == "" {
		return errors.New("runtime-group ID and Worker ID are required")
	}
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return err
	}
	return m.control.StopWorker(ctx, inspection.Spec, workerID, immediate)
}
func (m *Manager) InvokeWorker(ctx context.Context, input nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult {
	if m.nodes == nil {
		return invocationFailure("unavailable", "node routing is unavailable")
	}
	if input.NodeID != m.nodes.LocalNodeID() {
		return m.nodes.InvokeWorker(ctx, input)
	}
	return m.InvokeLocalWorker(ctx, input)
}

func (m *Manager) InvokeLocalWorker(ctx context.Context, input nodes.WorkerInvocationRequest) nodes.WorkerInvocationResult {
	if m.nodes == nil || input.NodeID != m.nodes.LocalNodeID() {
		return invocationFailure("target_mismatch", "target node does not match this kernel")
	}
	if input.SandboxID == "" || input.WorkerID == "" || input.Function == "" || len(input.Function) > 128 {
		return invocationFailure("invalid_request", "exact Worker target and registered function are required")
	}
	encoded, err := json.Marshal(input.Input)
	if err != nil || len(encoded) > 1<<20 {
		return invocationFailure("invalid_request", "Worker invocation input is invalid or exceeds 1 MiB")
	}
	inspection, err := m.sandboxes.Inspect(ctx, input.SandboxID)
	if err != nil {
		return invocationFailure("target_not_found", fmt.Sprintf("sandbox %q is unavailable", input.SandboxID))
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return invocationFailure("target_not_found", err.Error())
	}
	if inspection.Spec.SandboxID != input.SandboxID {
		return invocationFailure("target_mismatch", "sandbox identity does not match target")
	}
	result, err := m.control.InvokeWorker(ctx, inspection.Spec, input.WorkerID, input.PersistentExecutionID, input.Function, input.Input)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return invocationFailure("timeout", "Worker invocation timed out")
		}
		return invocationFailure("target_not_found", err.Error())
	}
	if result.OK {
		encoded, encodeErr := json.Marshal(result.Output)
		if encodeErr != nil || len(encoded) > 1<<20 {
			return invocationFailure("unavailable", "Worker invocation output is invalid or exceeds 1 MiB")
		}
		return nodes.WorkerInvocationResult{OK: true, Output: result.Output}
	}
	if result.Error == nil {
		return invocationFailure("unavailable", "Worker returned an invalid invocation result")
	}
	return nodes.WorkerInvocationResult{Error: &nodes.WorkerInvocationError{Code: result.Error.Code, Message: result.Error.Message}}
}

func invocationFailure(code, message string) nodes.WorkerInvocationResult {
	return nodes.WorkerInvocationResult{Error: &nodes.WorkerInvocationError{Code: code, Message: message}}
}
func (m *Manager) RunJob(ctx context.Context, workerID string, arguments []any, secrets map[string]string, checkModules []string) (supervisor.JobResult, error) {
	record, spec, err := m.find(ctx, workerID)
	if err != nil {
		return supervisor.JobResult{}, err
	}
	if record.WorkloadType != model.WorkloadJob {
		return supervisor.JobResult{}, errors.New("Worker is not a job")
	}
	for _, module := range checkModules {
		if err := validateCheckModule(spec, module); err != nil {
			return supervisor.JobResult{}, fmt.Errorf("type-check module: %w", err)
		}
	}
	return m.control.RunJob(ctx, spec, workerID, arguments, secrets, checkModules)
}
func (m *Manager) ConfigureService(ctx context.Context, runtimeGroupID, serviceID string, workerIDs []string, concurrencyPerWorker int) error {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return err
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return err
	}
	if inspection.Spec.WorkloadType != model.WorkloadService {
		return errors.New("runtime group is not a service group")
	}
	return m.control.ConfigureService(ctx, inspection.Spec, serviceID, workerIDs, concurrencyPerWorker)
}
func (m *Manager) ServiceOpenAPI(ctx context.Context, runtimeGroupID, serviceID string) (map[string]any, error) {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return nil, err
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return nil, err
	}
	if inspection.Spec.WorkloadType != model.WorkloadService {
		return nil, errors.New("runtime group is not a service group")
	}
	return m.control.ServiceOpenAPI(ctx, inspection.Spec, serviceID)
}
func (m *Manager) DispatchService(ctx context.Context, runtimeGroupID, serviceID string, request *http.Request) (*http.Response, error) {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return nil, err
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return nil, err
	}
	return m.control.DispatchService(ctx, inspection.Spec, serviceID, request)
}

func (m *Manager) ProxyServiceWebSocket(ctx context.Context, runtimeGroupID, serviceID string, writer http.ResponseWriter, request *http.Request) error {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return err
	}
	if err := requireWorkerRuntime(inspection); err != nil {
		return err
	}
	if inspection.Spec.WorkloadType != model.WorkloadService {
		return errors.New("runtime group is not a service group")
	}
	return m.control.ProxyServiceWebSocket(ctx, inspection.Spec, serviceID, writer, request)
}

func (m *Manager) find(ctx context.Context, workerID string) (Record, model.SandboxSpec, error) {
	record, err := m.Inspect(ctx, workerID)
	if err != nil {
		return Record{}, model.SandboxSpec{}, err
	}
	inspection, err := m.sandboxes.Inspect(ctx, record.RuntimeGroupID)
	if err != nil {
		return Record{}, model.SandboxSpec{}, err
	}
	return record, inspection.Spec, nil
}

func validateCheckModule(spec model.SandboxSpec, module string) error {
	if !filepath.IsAbs(module) {
		return validateEntrypoint(spec, module)
	}
	path := filepath.Clean(module)
	for _, allowed := range spec.Permissions.ReadPaths {
		if beneath(path, allowed) {
			return nil
		}
	}
	return fmt.Errorf("module %q is outside the parent read envelope", module)
}

func requireWorkerRuntime(inspection manager.Inspection) error {
	switch inspection.Status.ObservedState {
	case model.StateReady, model.StateActive, model.StateDraining:
		return nil
	default:
		return fmt.Errorf("%w: runtime group %s is %s", ErrRuntimeUnavailable, inspection.Spec.RuntimeGroupID, inspection.Status.ObservedState)
	}
}

func validateEntrypoint(spec model.SandboxSpec, entrypoint string) error {
	parsed, err := url.Parse(entrypoint)
	if err != nil || parsed.Scheme == "" {
		return errors.New("Worker entrypoint must be an absolute file or HTTPS URL")
	}
	switch parsed.Scheme {
	case "file":
		path := filepath.Clean(parsed.Path)
		for _, allowed := range spec.Permissions.ReadPaths {
			if beneath(path, allowed) {
				return nil
			}
		}
		return fmt.Errorf("entrypoint %q is outside the parent read envelope", entrypoint)
	case "https":
		if spec.DependencyMode != model.DependencyOnline {
			return errors.New("remote entrypoint requires online dependency mode")
		}
		for _, host := range spec.Permissions.ImportHosts {
			if parsed.Host == host || parsed.Hostname() == host {
				return nil
			}
		}
		return fmt.Errorf("entrypoint host %q is outside the import envelope", parsed.Host)
	default:
		return fmt.Errorf("unsupported entrypoint scheme %q", parsed.Scheme)
	}
}
func validatePermissions(parent model.Permissions, worker supervisor.WorkerPermissions) error {
	for _, check := range []struct {
		name               string
		requested, allowed []string
		paths              bool
	}{{"read", worker.Read, parent.ReadPaths, true}, {"write", worker.Write, parent.WritePaths, true}, {"network", worker.Net, parent.NetworkHosts, false}, {"import", worker.Import, parent.ImportHosts, false}, {"environment", worker.Env, parent.Environment, false}} {
		for _, requested := range check.requested {
			allowed := false
			for _, candidate := range check.allowed {
				if requested == candidate || (check.paths && beneath(requested, candidate)) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("Worker %s permission %q exceeds parent envelope", check.name, requested)
			}
		}
	}
	if len(worker.Sys) > 0 && !parent.SystemInfo {
		return errors.New("Worker system-information permission exceeds parent envelope")
	}
	return nil
}
func beneath(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path == root {
		return true
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
