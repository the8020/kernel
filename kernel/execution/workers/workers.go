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
}

type Control interface {
	Workers(context.Context, model.SandboxSpec) ([]supervisor.WorkerStatus, error)
	StartWorker(context.Context, model.SandboxSpec, supervisor.StartWorkerRequest) (supervisor.WorkerStatus, error)
	StopWorker(context.Context, model.SandboxSpec, string, bool) error
	InvokeWorker(context.Context, model.SandboxSpec, string, string, any) (supervisor.WorkerInvocationResult, error)
	RunJob(context.Context, model.SandboxSpec, string, any) (supervisor.JobResult, error)
	ConfigureService(context.Context, model.SandboxSpec, string, []string, int) error
	ServiceOpenAPI(context.Context, model.SandboxSpec, string) (map[string]any, error)
	DispatchService(context.Context, model.SandboxSpec, string, *http.Request) (*http.Response, error)
	ProxyServiceWebSocket(context.Context, model.SandboxSpec, string, http.ResponseWriter, *http.Request) error
}

type Manager struct {
	sandboxes      Sandboxes
	control        Control
	maximumWorkers int
	nodes          interface {
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

func New(sandboxes Sandboxes, control Control, maximumWorkers ...int) (*Manager, error) {
	if sandboxes == nil || control == nil {
		return nil, errors.New("sandbox catalog and supervisor control are required")
	}
	limit := 0
	if len(maximumWorkers) > 0 {
		limit = maximumWorkers[0]
	}
	if limit < 0 {
		return nil, errors.New("maximum node Worker count cannot be negative")
	}
	return &Manager{sandboxes: sandboxes, control: control, maximumWorkers: limit}, nil
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
			return Record{}, fmt.Errorf("node Worker capacity exhausted: %d of %d Workers are running", len(live), m.maximumWorkers)
		}
	}
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return Record{}, err
	}
	if request.Metadata.WorkloadType != inspection.Spec.WorkloadType {
		return Record{}, errors.New("Worker workload type does not match runtime group")
	}
	if request.Metadata.DebuggerName == "" {
		request.Metadata.DebuggerName = fmt.Sprintf("%s:%s:%s:%s", request.Metadata.WorkloadType, request.Metadata.OwnerID, request.Metadata.ExecutionID, request.Metadata.WorkerID)
	}
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
	if inspection.Spec.SandboxID != input.SandboxID {
		return invocationFailure("target_mismatch", "sandbox identity does not match target")
	}
	result, err := m.control.InvokeWorker(ctx, inspection.Spec, input.WorkerID, input.Function, input.Input)
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
func (m *Manager) RunJob(ctx context.Context, workerID string, input any) (supervisor.JobResult, error) {
	record, spec, err := m.find(ctx, workerID)
	if err != nil {
		return supervisor.JobResult{}, err
	}
	if record.WorkloadType != model.WorkloadJob {
		return supervisor.JobResult{}, errors.New("Worker is not a job")
	}
	return m.control.RunJob(ctx, spec, workerID, input)
}
func (m *Manager) ConfigureService(ctx context.Context, runtimeGroupID, serviceID string, workerIDs []string, maximumInFlight int) error {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
		return err
	}
	if inspection.Spec.WorkloadType != model.WorkloadService {
		return errors.New("runtime group is not a service group")
	}
	return m.control.ConfigureService(ctx, inspection.Spec, serviceID, workerIDs, maximumInFlight)
}
func (m *Manager) ServiceOpenAPI(ctx context.Context, runtimeGroupID, serviceID string) (map[string]any, error) {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
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
	return m.control.DispatchService(ctx, inspection.Spec, serviceID, request)
}

func (m *Manager) ProxyServiceWebSocket(ctx context.Context, runtimeGroupID, serviceID string, writer http.ResponseWriter, request *http.Request) error {
	inspection, err := m.sandboxes.Inspect(ctx, runtimeGroupID)
	if err != nil {
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
