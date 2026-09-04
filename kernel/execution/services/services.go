// Package services owns sandbox-local service Worker pools and scaling.
package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"the8020/kernel/execution/coordinator"
	executionprofile "the8020/kernel/execution/profile"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type GroupCoordinator interface {
	Ensure(context.Context, coordinator.Request) (manager.Inspection, error)
	Release(context.Context, string, string, string) error
}
type WorkerManager interface {
	Start(context.Context, string, supervisor.StartWorkerRequest) (workers.Record, error)
	List(context.Context, string) ([]workers.Record, error)
	StopInGroup(context.Context, string, string, bool) error
	ConfigureService(context.Context, string, string, []string, int) error
	ServiceOpenAPI(context.Context, string, string) (map[string]any, error)
	DispatchService(context.Context, string, string, *http.Request) (*http.Response, error)
	ProxyServiceWebSocket(context.Context, string, string, http.ResponseWriter, *http.Request) error
}
type Policy struct {
	Strategy        model.GroupingStrategy
	Profile         model.RuntimeProfile
	Resources       model.ResourceLimits
	Lifecycle       model.LifecyclePolicy
	WorkspaceMounts executionprofile.MountPolicy
	Logger          *slog.Logger
}
type Options struct {
	GroupKey             string
	Namespace            string
	MinimumWorkers       int
	MaximumWorkers       int
	ConcurrencyPerWorker int
	WorkerKeepAlive      time.Duration
	Permissions          *supervisor.WorkerPermissions
	ReleaseID            string
	Workspace            string
	WorkspaceWritable    bool
	LogicalServiceID     string
	Generation           uint64
	CanonicalBasePath    string
	OpenAPI              supervisor.OpenAPIMetadata
	ValidateEntrypoint   bool
	SandboxIndex         int
	DependencyMode       model.DependencyMode
	ExecutionMode        string
	TargetUtilization    float64
	PlacementWorkers     int
}
type Record struct {
	ServiceID            string                       `json:"service_id"`
	Entrypoint           string                       `json:"entrypoint"`
	RuntimeGroupID       string                       `json:"runtime_group_id,omitempty"`
	SandboxID            string                       `json:"sandbox_id,omitempty"`
	SandboxIP            string                       `json:"sandbox_ip,omitempty"`
	WorkerIDs            []string                     `json:"worker_ids"`
	ReleaseID            string                       `json:"release_id"`
	State                string                       `json:"state"`
	MinimumWorkers       int                          `json:"minimum_workers"`
	MaximumWorkers       int                          `json:"maximum_workers"`
	ConcurrencyPerWorker int                          `json:"concurrency_per_worker"`
	WorkerKeepAlive      time.Duration                `json:"worker_keep_alive"`
	StartedAt            time.Time                    `json:"started_at"`
	Failure              string                       `json:"failure,omitempty"`
	RuntimeUnavailable   bool                         `json:"runtime_unavailable,omitempty"`
	Permissions          supervisor.WorkerPermissions `json:"permissions"`
	LogicalServiceID     string                       `json:"logical_service_id,omitempty"`
	Generation           uint64                       `json:"generation,omitempty"`
	CanonicalBasePath    string                       `json:"canonical_base_path,omitempty"`
	OpenAPI              supervisor.OpenAPIMetadata   `json:"openapi,omitempty"`
	ValidateEntrypoint   bool                         `json:"validate_entrypoint,omitempty"`
	SandboxIndex         int                          `json:"sandbox_index"`
	ExecutionMode        string                       `json:"execution_mode,omitempty"`
	TargetUtilization    float64                      `json:"target_utilization,omitempty"`
	OccupiedSlots        int                          `json:"-"`
	CapacitySlots        int                          `json:"-"`
}

var ErrSandboxCapacity = errors.New("service sandbox has no target capacity")

var ErrInvalidServiceDefinition = errors.New("service definition was rejected by the runtime")

type invalidServiceDefinitionError struct{ cause error }

func (e *invalidServiceDefinitionError) Error() string { return e.cause.Error() }
func (e *invalidServiceDefinitionError) Unwrap() []error {
	return []error{ErrInvalidServiceDefinition, e.cause}
}

type SandboxCapacityError struct {
	Occupied int
	Slots    int
	Reason   string
}

func (e *SandboxCapacityError) Error() string {
	return fmt.Sprintf("%s: %d of %d execution slots occupied", e.Reason, e.Occupied, e.Slots)
}

func (e *SandboxCapacityError) Unwrap() error { return ErrSandboxCapacity }

type Manager struct {
	mu          sync.Mutex
	coordinator GroupCoordinator
	workers     WorkerManager
	store       *records.Store
	policy      Policy
	now         func() time.Time
	logger      *slog.Logger
}

func New(groupCoordinator GroupCoordinator, workerManager WorkerManager, store *records.Store, policy Policy) (*Manager, error) {
	if groupCoordinator == nil || workerManager == nil || store == nil {
		return nil, errors.New("coordinator, Worker manager, and service store are required")
	}
	if policy.Strategy == "" {
		policy.Strategy = model.GroupingOwner
	}
	if !policy.Strategy.Valid() {
		return nil, errors.New("valid service grouping strategy is required")
	}
	return &Manager{coordinator: groupCoordinator, workers: workerManager, store: store, policy: policy, now: func() time.Time { return time.Now().UTC() }, logger: policy.Logger}, nil
}

func (m *Manager) Start(ctx context.Context, serviceID, entrypoint string, options Options) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if serviceID == "" || entrypoint == "" {
		return Record{}, errors.New("service ID and entrypoint are required")
	}
	if options.ExecutionMode != "stateless" && options.ExecutionMode != "persistent" {
		return Record{}, errors.New("service execution mode must be stateless or persistent")
	}
	if options.TargetUtilization <= 0 || options.TargetUtilization > 1 {
		return Record{}, errors.New("service target utilization must be greater than zero and at most one")
	}
	baseProfile := m.policy.Profile
	if options.DependencyMode != "" {
		baseProfile.DependencyMode = options.DependencyMode
	}
	profile, err := executionprofile.ForWorkerWithWorkspace(baseProfile, options.Permissions, executionprofile.Workspace{Source: options.Workspace, OwnerID: serviceID, Writable: options.WorkspaceWritable}, m.policy.WorkspaceMounts)
	if err != nil {
		return Record{}, err
	}
	existing, loadErr := m.inspect(serviceID)
	if loadErr == nil && existing.State != "STOPPED" && existing.State != "FAILED" {
		return Record{}, fmt.Errorf("service %s is already started", serviceID)
	}
	if loadErr == nil && existing.State == "FAILED" && existing.RuntimeUnavailable {
		if err := m.releaseSandbox(ctx, existing); err != nil {
			return existing, fmt.Errorf("release unavailable runtime group: %w", err)
		}
	}
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		m.quarantineInvalidRecord(serviceID, loadErr)
	}
	minimum := options.MinimumWorkers
	maximum := options.MaximumWorkers
	concurrency := options.ConcurrencyPerWorker
	placementWorkers := options.PlacementWorkers
	if minimum < 0 || maximum < 1 || minimum > maximum || concurrency < 1 || placementWorkers < minimum || placementWorkers > maximum || options.WorkerKeepAlive <= 0 {
		return Record{}, errors.New("invalid service Worker limits")
	}
	if options.ReleaseID == "" {
		options.ReleaseID = "development"
	}
	record := Record{ServiceID: serviceID, LogicalServiceID: options.LogicalServiceID, Generation: options.Generation, CanonicalBasePath: options.CanonicalBasePath, OpenAPI: options.OpenAPI, ValidateEntrypoint: options.ValidateEntrypoint, SandboxIndex: options.SandboxIndex, Entrypoint: entrypoint, ReleaseID: options.ReleaseID, State: "STARTING", MinimumWorkers: minimum, MaximumWorkers: maximum, ConcurrencyPerWorker: concurrency, WorkerKeepAlive: options.WorkerKeepAlive, StartedAt: m.now(), ExecutionMode: options.ExecutionMode, TargetUtilization: options.TargetUtilization}
	if record.LogicalServiceID == "" {
		record.LogicalServiceID = serviceID
	}
	if err := m.store.Save(serviceID, record); err != nil {
		return Record{}, err
	}
	executionID, err := model.NewID("execution")
	if err != nil {
		return m.failUnowned(record, err)
	}
	placementGroup := options.GroupKey
	group, err := m.coordinator.Ensure(ctx, coordinator.Request{WorkloadType: model.WorkloadService, OwnerID: serviceID, ExecutionID: executionID, Namespace: options.Namespace, PlacementGroup: &placementGroup, LogicalServiceID: record.LogicalServiceID, RequestedWorkers: placementWorkers, Strategy: m.policy.Strategy, Profile: profile, ResourceLimits: m.policy.Resources, Lifecycle: m.policy.Lifecycle})
	if err != nil {
		return m.failUnowned(record, err)
	}
	record.RuntimeGroupID, record.SandboxID, record.SandboxIP = group.Spec.RuntimeGroupID, group.Spec.SandboxID, group.Spec.Network.SandboxIP
	permissions := permissionsFor(group.Spec.Permissions)
	if options.Permissions != nil {
		permissions = *options.Permissions
	}
	record.Permissions = permissions
	for len(record.WorkerIDs) < minimum {
		workerID, workerErr := m.startWorker(ctx, record, permissions)
		if workerErr != nil {
			return m.failStart(record, workerErr)
		}
		record.WorkerIDs = append(record.WorkerIDs, workerID)
	}
	configureErr := m.workers.ConfigureService(ctx, record.RuntimeGroupID, serviceID, record.WorkerIDs, record.ConcurrencyPerWorker)
	if configureErr != nil {
		return m.failStart(record, configureErr)
	}
	record.State = workerState(len(record.WorkerIDs))
	if err := m.store.Save(serviceID, record); err != nil {
		return m.failStart(record, err)
	}
	return record, nil
}

func (m *Manager) Scale(ctx context.Context, serviceID string, count int) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scaleLocked(ctx, serviceID, count)
}

func (m *Manager) scaleLocked(ctx context.Context, serviceID string, count int) (Record, error) {
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if count < 0 || count > record.MaximumWorkers {
		return record, fmt.Errorf("Worker count must be between 0 and %d", record.MaximumWorkers)
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		return m.failUnavailableLocked(record, err)
	}
	return m.scaleRecordLocked(ctx, record, count, live)
}

func (m *Manager) scaleRecordLocked(ctx context.Context, record Record, count int, live []workers.Record) (Record, error) {
	record, cleanupErr, err := m.reconcileWorkerSetLocked(ctx, record, live)
	if err != nil {
		return record, err
	}
	desiredState := workerState(count)
	if len(record.WorkerIDs) == count && record.State == desiredState && record.Failure == "" {
		return record, cleanupErr
	}
	permissions := record.Permissions
	if len(permissions.Read)+len(permissions.Write)+len(permissions.Net)+len(permissions.Import)+len(permissions.Env)+len(permissions.Sys) == 0 {
		permissions = permissionsFor(m.policy.Profile.Permissions)
	}
	previousWorkerIDs := append([]string(nil), record.WorkerIDs...)
	for len(record.WorkerIDs) < count {
		workerID, startErr := m.startWorker(ctx, record, permissions)
		if startErr != nil {
			for _, id := range record.WorkerIDs[len(previousWorkerIDs):] {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, id, true)
			}
			return record, errors.Join(cleanupErr, startErr)
		}
		record.WorkerIDs = append(record.WorkerIDs, workerID)
	}
	sort.Strings(record.WorkerIDs)
	var removed []string
	allWorkerIDs := append([]string(nil), record.WorkerIDs...)
	desiredWorkerIDs := append([]string(nil), record.WorkerIDs...)
	if len(desiredWorkerIDs) > count {
		inFlight := make(map[string]int, len(live))
		for _, item := range live {
			inFlight[item.Worker.WorkerID] = item.Worker.InFlight
		}
		removeSet := map[string]bool{}
		for index := len(desiredWorkerIDs) - 1; index >= 0 && len(removeSet) < len(desiredWorkerIDs)-count; index-- {
			workerID := desiredWorkerIDs[index]
			if inFlight[workerID] == 0 {
				removeSet[workerID] = true
				removed = append(removed, workerID)
			}
		}
		if len(removeSet) != len(desiredWorkerIDs)-count {
			return record, errors.Join(cleanupErr, errors.New("cannot remove Workers with occupied execution slots"))
		}
		desiredWorkerIDs = make([]string, 0, count)
		for _, workerID := range allWorkerIDs {
			if !removeSet[workerID] {
				desiredWorkerIDs = append(desiredWorkerIDs, workerID)
			}
		}
	}
	if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, record.ServiceID, desiredWorkerIDs, record.ConcurrencyPerWorker); err != nil {
		for _, id := range record.WorkerIDs {
			if !contains(previousWorkerIDs, id) {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, id, true)
			}
		}
		return record, errors.Join(cleanupErr, err)
	}
	record.WorkerIDs = desiredWorkerIDs
	record.State = desiredState
	record.Failure = ""
	if err := m.store.Save(record.ServiceID, record); err != nil {
		_ = m.workers.ConfigureService(context.Background(), record.RuntimeGroupID, record.ServiceID, previousWorkerIDs, record.ConcurrencyPerWorker)
		for _, id := range desiredWorkerIDs {
			if !contains(previousWorkerIDs, id) {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, id, true)
			}
		}
		return record, errors.Join(cleanupErr, err)
	}
	for _, id := range removed {
		if err := m.workers.StopInGroup(ctx, record.RuntimeGroupID, id, false); err != nil {
			return record, errors.Join(cleanupErr, err)
		}
	}
	return record, cleanupErr
}

// reconcileWorkerSetLocked removes recorded Workers that the supervisor no
// longer considers ready and reaps Workers left behind by an interrupted pool
// mutation. The supervisor pool is narrowed before any Worker is stopped so a
// failed or draining Worker cannot receive another request.
func (m *Manager) reconcileWorkerSetLocked(ctx context.Context, record Record, live []workers.Record) (Record, error, error) {
	liveByID := make(map[string]workers.Record, len(live))
	for _, item := range live {
		liveByID[item.Worker.WorkerID] = item
	}
	recorded := make(map[string]bool, len(record.WorkerIDs))
	ready := make([]string, 0, len(record.WorkerIDs))
	toStop := make(map[string]workers.Record)
	for _, workerID := range record.WorkerIDs {
		recorded[workerID] = true
		item, exists := liveByID[workerID]
		if exists && item.Worker.State == "ready" {
			ready = append(ready, workerID)
			continue
		}
		if exists {
			toStop[workerID] = item
		}
	}
	for _, item := range live {
		workerID := item.Worker.WorkerID
		if item.Worker.WorkloadID == record.ServiceID && !recorded[workerID] {
			toStop[workerID] = item
		}
	}
	if len(ready) == len(record.WorkerIDs) && len(toStop) == 0 {
		return record, nil, nil
	}
	sort.Strings(ready)
	if len(ready) != len(record.WorkerIDs) {
		if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, record.ServiceID, ready, record.ConcurrencyPerWorker); err != nil {
			return record, nil, fmt.Errorf("exclude unavailable service Workers: %w", err)
		}
		record.WorkerIDs = ready
		if err := m.store.Save(record.ServiceID, record); err != nil {
			return record, nil, fmt.Errorf("persist available service Workers: %w", err)
		}
	}
	stopIDs := make([]string, 0, len(toStop))
	for workerID := range toStop {
		stopIDs = append(stopIDs, workerID)
	}
	sort.Strings(stopIDs)
	var cleanupErr error
	for _, workerID := range stopIDs {
		item := toStop[workerID]
		immediate := item.Worker.State == "failed"
		if err := m.workers.StopInGroup(ctx, record.RuntimeGroupID, workerID, immediate); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("reap unavailable Worker %s: %w", workerID, err))
		}
	}
	return record, cleanupErr, nil
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// Stop removes idle Workers and reports whether the pool is fully retired.
// Occupied Workers remain durably DRAINING for a later reconciliation pass.
func (m *Manager) Stop(ctx context.Context, serviceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return false, err
	}
	if record.State == "STOPPED" && len(record.WorkerIDs) == 0 {
		if err := m.releaseSandbox(ctx, record); err != nil {
			return false, err
		}
		return true, nil
	}
	if record.State == "FAILED" && record.RuntimeUnavailable {
		record.WorkerIDs = nil
		record.State = "STOPPED"
		if err := m.store.Save(serviceID, record); err != nil {
			return false, err
		}
		if err := m.releaseSandbox(ctx, record); err != nil {
			return false, err
		}
		return true, nil
	}
	record.State = "DRAINING"
	record.Failure = ""
	if err := m.store.Save(serviceID, record); err != nil {
		return false, err
	}
	if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, serviceID, nil, record.ConcurrencyPerWorker); err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, workers.ErrRuntimeUnavailable) {
			record.Failure = err.Error()
			_ = m.store.Save(serviceID, record)
			return false, err
		}
		record.WorkerIDs = nil
		record.State = "STOPPED"
		record.Failure = ""
		if err := m.store.Save(serviceID, record); err != nil {
			return false, err
		}
		if err := m.releaseSandbox(ctx, record); err != nil {
			return false, err
		}
		return true, nil
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, workers.ErrRuntimeUnavailable) {
			return false, err
		}
		// Runtime records are recoverable indexes, not authority over a
		// sandbox. Startup may already have removed the inherited group, so
		// retire this pool without requiring its vanished Workers to answer.
		record.WorkerIDs = nil
		record.State = "STOPPED"
		record.Failure = ""
		if err := m.store.Save(serviceID, record); err != nil {
			return false, err
		}
		if err := m.releaseSandbox(ctx, record); err != nil {
			return false, err
		}
		return true, nil
	}
	liveByID := make(map[string]workers.Record, len(live))
	for _, item := range live {
		liveByID[item.Worker.WorkerID] = item
	}
	remaining := record.WorkerIDs[:0]
	for _, id := range record.WorkerIDs {
		if _, exists := liveByID[id]; exists {
			remaining = append(remaining, id)
		}
	}
	record.WorkerIDs = append([]string(nil), remaining...)
	if err := m.store.Save(serviceID, record); err != nil {
		return false, err
	}
	var joined error
	remaining = append([]string(nil), record.WorkerIDs...)
	for _, id := range append([]string(nil), record.WorkerIDs...) {
		if liveByID[id].Worker.InFlight > 0 {
			continue
		}
		if stopErr := m.workers.StopInGroup(ctx, record.RuntimeGroupID, id, false); stopErr != nil {
			joined = errors.Join(joined, fmt.Errorf("stop Worker %s: %w", id, stopErr))
			continue
		}
		remaining = remove(remaining, id)
		record.WorkerIDs = append([]string(nil), remaining...)
		if saveErr := m.store.Save(serviceID, record); saveErr != nil {
			joined = errors.Join(joined, saveErr)
			break
		}
	}
	if joined != nil {
		record.Failure = joined.Error()
		_ = m.store.Save(serviceID, record)
		return false, joined
	}
	if len(record.WorkerIDs) > 0 {
		return false, nil
	}
	record.WorkerIDs = nil
	record.State = "STOPPED"
	record.Failure = ""
	if err := m.store.Save(serviceID, record); err != nil {
		return false, err
	}
	if err := m.releaseSandbox(ctx, record); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveStopped deletes one fully retired pool record. It deliberately refuses
// to remove live ownership so callers cannot turn record cleanup into an
// untracked running sandbox.
func (m *Manager) RemoveStopped(serviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return err
	}
	if record.State != "STOPPED" || len(record.WorkerIDs) != 0 {
		return fmt.Errorf("service %s is not fully stopped", serviceID)
	}
	return m.store.Delete(serviceID)
}

func (m *Manager) releaseSandbox(ctx context.Context, record Record) error {
	if record.RuntimeGroupID == "" {
		return nil
	}
	err := m.coordinator.Release(ctx, record.RuntimeGroupID, record.ServiceID, record.LogicalServiceID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func remove(values []string, candidate string) []string {
	result := values[:0]
	for _, value := range values {
		if value != candidate {
			result = append(result, value)
		}
	}
	return result
}
func (m *Manager) List() ([]Record, error) {
	ids, err := m.store.IDs()
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0, len(ids))
	for _, id := range ids {
		record, ok := m.recoverableRecord(id)
		if !ok {
			continue
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServiceID < result[j].ServiceID })
	return result, nil
}
func (m *Manager) Inspect(serviceID string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inspect(serviceID)
}

func (m *Manager) OpenAPI(ctx context.Context, serviceID string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return nil, err
	}
	if record.State != "READY" || len(record.WorkerIDs) == 0 {
		return nil, fmt.Errorf("service %s has no ready Worker", serviceID)
	}
	document, err := m.workers.ServiceOpenAPI(ctx, record.RuntimeGroupID, record.ServiceID)
	if err != nil {
		_, err = m.failUnavailableLocked(record, err)
	}
	return document, err
}

// FailGroup marks every live service pool in a failed runtime group without
// attempting Worker operations against the terminated sandbox.
func (m *Manager) FailGroup(runtimeGroupID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, err := m.store.IDs()
	if err != nil {
		return err
	}
	var joined error
	for _, id := range ids {
		record, ok := m.recoverableRecord(id)
		if !ok {
			continue
		}
		if record.RuntimeGroupID != runtimeGroupID || (record.State != "STARTING" && record.State != "READY" && record.State != "IDLE") {
			continue
		}
		record.State = "FAILED"
		record.Failure = reason
		record.RuntimeUnavailable = true
		joined = errors.Join(joined, m.store.Save(record.ServiceID, record))
	}
	return joined
}

// RetireUnavailable converts one pool whose sandbox was authoritatively absent
// during startup reconciliation into a terminal record. It performs no Worker
// or sandbox calls because those resources are already known not to exist.
func (m *Manager) RetireUnavailable(serviceID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return err
	}
	if record.State == "STOPPED" && len(record.WorkerIDs) == 0 {
		return nil
	}
	record.WorkerIDs = nil
	record.State = "STOPPED"
	record.Failure = reason
	record.RuntimeUnavailable = true
	return m.store.Save(record.ServiceID, record)
}

// Restore verifies that persisted pools still refer to ready supervisor Workers.
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, err := m.store.IDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, ok := m.recoverableRecord(id)
		if !ok {
			continue
		}
		if record.State != "READY" && record.State != "IDLE" {
			continue
		}
		listed, listErr := m.workers.List(ctx, record.RuntimeGroupID)
		if listErr != nil || !containsEveryWorker(record.WorkerIDs, listed) {
			record.State = "FAILED"
			if listErr != nil {
				record.Failure = "restore runtime group: " + listErr.Error()
			} else {
				record.Failure = "restore runtime group: persisted Workers are unavailable"
			}
			m.persistRestoreFailure(record, nil)
			continue
		}
	}
	return nil
}

func containsEveryWorker(expected []string, listed []workers.Record) bool {
	available := make(map[string]bool, len(listed))
	for _, item := range listed {
		available[item.Worker.WorkerID] = item.Worker.State == "ready"
	}
	for _, workerID := range expected {
		if !available[workerID] {
			return false
		}
	}
	return true
}
func (m *Manager) inspect(serviceID string) (Record, error) {
	var record Record
	if err := m.store.Load(serviceID, &record); err != nil {
		return record, fmt.Errorf("service %q: %w", serviceID, err)
	}
	if record.ServiceID == "" || record.ServiceID != serviceID {
		return record, fmt.Errorf("service %q: persisted identity is %q", serviceID, record.ServiceID)
	}
	if record.ExecutionMode != "stateless" && record.ExecutionMode != "persistent" {
		return record, fmt.Errorf("service %q: unsupported execution mode %q", serviceID, record.ExecutionMode)
	}
	if record.LogicalServiceID == "" {
		return record, fmt.Errorf("service %q: logical service identity is missing", serviceID)
	}
	if record.MinimumWorkers < 0 || record.MaximumWorkers < 1 || record.MinimumWorkers > record.MaximumWorkers || record.ConcurrencyPerWorker < 1 {
		return record, fmt.Errorf("service %q: invalid persisted Worker limits", serviceID)
	}
	if record.TargetUtilization <= 0 || record.TargetUtilization > 1 || record.WorkerKeepAlive <= 0 {
		return record, fmt.Errorf("service %q: invalid persisted scaling policy", serviceID)
	}
	return record, nil
}

func (m *Manager) recoverableRecord(serviceID string) (Record, bool) {
	record, err := m.inspect(serviceID)
	if err == nil {
		return record, true
	}
	m.quarantineInvalidRecord(serviceID, err)
	return Record{}, false
}

func (m *Manager) quarantineInvalidRecord(serviceID string, cause error) {
	path, err := m.store.Quarantine(serviceID)
	if m.logger == nil {
		return
	}
	if err != nil {
		m.logger.Error("skip invalid service-pool record; quarantine failed", "service_pool_id", serviceID, "error", cause, "quarantine_error", err)
		return
	}
	m.logger.Error("quarantined invalid service-pool record", "service_pool_id", serviceID, "path", path, "error", cause)
}

func (m *Manager) persistRestoreFailure(record Record, cause error) {
	if cause != nil {
		record.State = "FAILED"
		record.Failure = cause.Error()
	}
	if err := m.store.Save(record.ServiceID, record); err != nil {
		if m.logger != nil {
			m.logger.Error("persist isolated service restore failure", "service_pool_id", record.ServiceID, "error", err)
		}
		return
	}
	if m.logger != nil {
		m.logger.Error("isolated service restore failure", "service_pool_id", record.ServiceID, "error", record.Failure)
	}
}
func (m *Manager) startWorker(ctx context.Context, record Record, permissions supervisor.WorkerPermissions) (string, error) {
	workerID, err := model.NewWorkerID()
	if err != nil {
		return "", err
	}
	executionID, err := model.NewID("execution")
	if err != nil {
		return "", err
	}
	started, err := m.workers.Start(ctx, record.RuntimeGroupID, supervisor.StartWorkerRequest{Metadata: supervisor.ExecutionMetadata{WorkerID: workerID, ExecutionID: executionID, WorkloadType: model.WorkloadService, OwnerID: record.LogicalServiceID, WorkloadID: record.ServiceID, ReleaseID: record.ReleaseID, Entrypoint: record.Entrypoint, DebuggerName: "service:" + record.LogicalServiceID + ":" + executionID + ":" + workerID, ValidateEntrypoint: record.ValidateEntrypoint, Service: &supervisor.ServiceExecutionMetadata{ServiceID: record.LogicalServiceID, Generation: record.Generation, CanonicalBasePath: record.CanonicalBasePath, OpenAPI: record.OpenAPI, ExecutionMode: record.ExecutionMode}}, Permissions: permissions})
	if err != nil {
		if supervisor.IsRequestRejected(err) {
			return "", &invalidServiceDefinitionError{cause: err}
		}
		return "", err
	}
	return started.Worker.WorkerID, nil
}

// Dispatch sends one streaming request through an existing service pool.
func (m *Manager) Dispatch(ctx context.Context, serviceID string, request *http.Request) (*http.Response, error) {
	var current Record
	var err error
	if request.Header.Get("X-80-20-Internal-Persistent-Existing") == "true" {
		current, err = m.Inspect(serviceID)
	} else {
		current, err = m.EnsureCapacity(ctx, serviceID, int(^uint(0)>>1), 0)
		var capacity *SandboxCapacityError
		if errors.As(err, &capacity) && capacity.Occupied < capacity.Slots {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	response, err := m.workers.DispatchService(ctx, current.RuntimeGroupID, current.ServiceID, request)
	if err != nil {
		return nil, err
	}
	workerID := response.Header.Get("X-80-20-Runtime-Worker-ID")
	response.Header.Del("X-80-20-Runtime-Worker-ID")
	if workerID != "" {
		response.Header.Set("X-80-20-Internal-Selected-Worker-ID", workerID)
	}
	return response, nil
}

func (m *Manager) ProxyWebSocket(ctx context.Context, serviceID string, writer http.ResponseWriter, request *http.Request) error {
	m.mu.Lock()
	record, err := m.inspect(serviceID)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if record.State != "READY" {
		return errors.New("service sandbox pool is unavailable")
	}
	return m.workers.ProxyServiceWebSocket(ctx, record.RuntimeGroupID, record.ServiceID, writer, request)
}

// EnsureCapacity grows one sandbox-local pool up to the supplied allowance and
// reports ErrSandboxCapacity when the kernel must place capacity elsewhere.
func (m *Manager) EnsureCapacity(ctx context.Context, serviceID string, growthLimit, occupiedFloor int) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if growthLimit < 0 || occupiedFloor < 0 {
		return record, errors.New("service sandbox growth limit and occupied-slot floor cannot be negative")
	}
	allowedMaximum := min(record.MaximumWorkers, max(growthLimit, len(record.WorkerIDs)))
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		return m.failUnavailableLocked(record, err)
	}
	if workerSetNeedsReconciliation(record, live) {
		target := len(record.WorkerIDs)
		record, err = m.scaleRecordLocked(ctx, record, target, live)
		if err != nil {
			return record, err
		}
		live, err = m.workers.List(ctx, record.RuntimeGroupID)
		if err != nil {
			return m.failUnavailableLocked(record, err)
		}
	}
	inFlight := map[string]int{}
	for _, item := range live {
		if item.Worker.State == "ready" {
			inFlight[item.Worker.WorkerID] = item.Worker.InFlight
		}
	}
	occupied := 0
	healthy := 0
	for _, id := range record.WorkerIDs {
		if value, ok := inFlight[id]; ok {
			healthy++
			occupied += value
		}
	}
	occupied = max(occupied, occupiedFloor)
	if healthy == 0 {
		if len(record.WorkerIDs) < allowedMaximum {
			result, scaleErr := m.scaleRecordLocked(ctx, record, min(allowedMaximum, max(record.MinimumWorkers, 1)), live)
			return withCapacitySnapshot(result, occupied), scaleErr
		}
		return record, &SandboxCapacityError{Occupied: 0, Slots: 0, Reason: "sandbox allocation has no healthy Workers"}
	}
	requiredWorkers := int(math.Ceil(float64(occupied+1) / (float64(record.ConcurrencyPerWorker) * record.TargetUtilization)))
	requiredWorkers = max(requiredWorkers, record.MinimumWorkers)
	if requiredWorkers > allowedMaximum {
		if len(record.WorkerIDs) < allowedMaximum {
			result, scaleErr := m.scaleRecordLocked(ctx, record, allowedMaximum, live)
			return withCapacitySnapshot(result, occupied), scaleErr
		}
		return record, &SandboxCapacityError{Occupied: occupied, Slots: healthy * record.ConcurrencyPerWorker, Reason: fmt.Sprintf("target utilization requires %d Workers but current sandbox allowance is %d", requiredWorkers, allowedMaximum)}
	}
	if requiredWorkers > len(record.WorkerIDs) {
		result, scaleErr := m.scaleRecordLocked(ctx, record, requiredWorkers, live)
		if scaleErr != nil && occupied < healthy*record.ConcurrencyPerWorker {
			return withCapacitySnapshot(result, occupied), &SandboxCapacityError{
				Occupied: occupied,
				Slots:    healthy * record.ConcurrencyPerWorker,
				Reason:   "target-utilization growth failed: " + scaleErr.Error(),
			}
		}
		return withCapacitySnapshot(result, occupied), scaleErr
	}
	return withCapacitySnapshot(record, occupied), nil
}

// ReconcileCapacity repairs a sandbox-local pool and applies kernel-owned scale-down
// hysteresis. Only idle Workers above the declared minimum are removable.
func (m *Manager) ReconcileCapacity(ctx context.Context, serviceID string, minimumWorkers int) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if minimumWorkers < 0 || minimumWorkers > record.MaximumWorkers {
		return record, fmt.Errorf("sandbox-local minimum Workers must be between 0 and %d", record.MaximumWorkers)
	}
	if record.MinimumWorkers != minimumWorkers {
		record.MinimumWorkers = minimumWorkers
		if err := m.store.Save(record.ServiceID, record); err != nil {
			return record, err
		}
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		return m.failUnavailableLocked(record, err)
	}
	if workerSetNeedsReconciliation(record, live) {
		record, err = m.scaleRecordLocked(ctx, record, len(record.WorkerIDs), live)
		if err != nil {
			return record, err
		}
		live, err = m.workers.List(ctx, record.RuntimeGroupID)
		if err != nil {
			return m.failUnavailableLocked(record, err)
		}
	}
	if len(record.WorkerIDs) < record.MinimumWorkers {
		return m.scaleRecordLocked(ctx, record, record.MinimumWorkers, live)
	}
	occupied, healthy := 0, 0
	for _, item := range live {
		if item.Worker.State == "ready" && slices.Contains(record.WorkerIDs, item.Worker.WorkerID) {
			healthy++
			occupied += item.Worker.InFlight
		}
	}
	utilization := 1.0
	if healthy > 0 {
		utilization = float64(occupied) / float64(healthy*record.ConcurrencyPerWorker)
	}
	if len(record.WorkerIDs) <= record.MinimumWorkers || utilization >= record.TargetUtilization {
		return withCapacitySnapshot(record, occupied), nil
	}
	type idleWorker struct {
		id    string
		since time.Time
	}
	eligible := make([]idleWorker, 0, len(record.WorkerIDs)-record.MinimumWorkers)
	now := m.now()
	for _, item := range live {
		worker := item.Worker
		if !slices.Contains(record.WorkerIDs, worker.WorkerID) || worker.State != "ready" || worker.InFlight != 0 || worker.IdleSinceMS <= 0 {
			continue
		}
		since := time.UnixMilli(worker.IdleSinceMS)
		if !now.Before(since.Add(record.WorkerKeepAlive)) {
			eligible = append(eligible, idleWorker{id: worker.WorkerID, since: since})
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].since.Before(eligible[j].since) || (eligible[i].since.Equal(eligible[j].since) && eligible[i].id < eligible[j].id)
	})
	requiredWorkers := int(math.Ceil(float64(occupied) / (float64(record.ConcurrencyPerWorker) * record.TargetUtilization)))
	requiredWorkers = max(requiredWorkers, record.MinimumWorkers)
	maximumRemovals := max(len(record.WorkerIDs)-requiredWorkers, 0)
	if len(eligible) > maximumRemovals {
		eligible = eligible[:maximumRemovals]
	}
	if len(eligible) == 0 {
		return withCapacitySnapshot(record, occupied), nil
	}
	removeIDs := make([]string, len(eligible))
	for index := range eligible {
		removeIDs[index] = eligible[index].id
	}
	result, err := m.removeIdleWorkersLocked(ctx, record, removeIDs)
	return withCapacitySnapshot(result, occupied), err
}

func (m *Manager) removeIdleWorkersLocked(ctx context.Context, record Record, removeIDs []string) (Record, error) {
	removeSet := make(map[string]bool, len(removeIDs))
	for _, workerID := range removeIDs {
		removeSet[workerID] = true
	}
	previous := append([]string(nil), record.WorkerIDs...)
	desired := make([]string, 0, len(previous)-len(removeSet))
	for _, workerID := range previous {
		if !removeSet[workerID] {
			desired = append(desired, workerID)
		}
	}
	if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, record.ServiceID, desired, record.ConcurrencyPerWorker); err != nil {
		return record, err
	}
	record.WorkerIDs = desired
	record.State = workerState(len(desired))
	if err := m.store.Save(record.ServiceID, record); err != nil {
		_ = m.workers.ConfigureService(context.Background(), record.RuntimeGroupID, record.ServiceID, previous, record.ConcurrencyPerWorker)
		return record, err
	}
	var joined error
	for _, workerID := range removeIDs {
		if err := m.workers.StopInGroup(ctx, record.RuntimeGroupID, workerID, false); err != nil {
			joined = errors.Join(joined, fmt.Errorf("stop idle Worker %s: %w", workerID, err))
		}
	}
	return record, joined
}

func withCapacitySnapshot(record Record, occupied int) Record {
	record.OccupiedSlots = occupied
	record.CapacitySlots = len(record.WorkerIDs) * record.ConcurrencyPerWorker
	return record
}

func workerState(count int) string {
	if count == 0 {
		return "IDLE"
	}
	return "READY"
}

func workerSetNeedsReconciliation(record Record, live []workers.Record) bool {
	liveByID := make(map[string]workers.Record, len(live))
	for _, item := range live {
		liveByID[item.Worker.WorkerID] = item
	}
	recorded := make(map[string]bool, len(record.WorkerIDs))
	for _, workerID := range record.WorkerIDs {
		recorded[workerID] = true
		item, exists := liveByID[workerID]
		if !exists || item.Worker.State != "ready" {
			return true
		}
	}
	for _, item := range live {
		if item.Worker.WorkloadID == record.ServiceID && !recorded[item.Worker.WorkerID] {
			return true
		}
	}
	return false
}

func (m *Manager) fail(record Record, cause error) (Record, error) {
	record.State = "FAILED"
	record.Failure = cause.Error()
	record.RuntimeUnavailable = false
	_ = m.store.Save(record.ServiceID, record)
	return record, cause
}

func (m *Manager) failUnavailableLocked(record Record, cause error) (Record, error) {
	if !errors.Is(cause, workers.ErrRuntimeUnavailable) {
		return record, cause
	}
	record.State = "FAILED"
	record.Failure = cause.Error()
	record.RuntimeUnavailable = true
	return record, errors.Join(cause, m.store.Save(record.ServiceID, record))
}

func (m *Manager) failStart(record Record, cause error) (Record, error) {
	remaining := make([]string, 0, len(record.WorkerIDs))
	var cleanupErr error
	for _, workerID := range record.WorkerIDs {
		if err := m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, workerID, true); err != nil {
			remaining = append(remaining, workerID)
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop failed-start Worker %s: %w", workerID, err))
		}
	}
	record.WorkerIDs = remaining
	if len(remaining) == 0 {
		cleanupErr = errors.Join(cleanupErr, m.releaseSandbox(context.Background(), record))
	}
	joined := errors.Join(cause, cleanupErr)
	record.State = "FAILED"
	record.Failure = joined.Error()
	record.RuntimeUnavailable = false
	if cleanupErr == nil {
		record.RuntimeGroupID = ""
		record.SandboxID = ""
		record.SandboxIP = ""
		if err := m.store.Delete(record.ServiceID); err == nil || errors.Is(err, os.ErrNotExist) {
			return record, cause
		} else {
			joined = errors.Join(joined, fmt.Errorf("remove failed-start pool record: %w", err))
			record.Failure = joined.Error()
		}
	}
	_ = m.store.Save(record.ServiceID, record)
	return record, joined
}

func (m *Manager) failUnowned(record Record, cause error) (Record, error) {
	record.State = "FAILED"
	record.Failure = cause.Error()
	return record, errors.Join(cause, m.store.Delete(record.ServiceID))
}
func permissionsFor(value model.Permissions) supervisor.WorkerPermissions {
	sys := []string(nil)
	if value.SystemInfo {
		sys = []string{"hostname", "osRelease"}
	}
	return supervisor.WorkerPermissions{Read: append([]string(nil), value.ReadPaths...), Write: append([]string(nil), value.WritePaths...), Net: append([]string(nil), value.NetworkHosts...), Import: append([]string(nil), value.ImportHosts...), Env: append([]string(nil), value.Environment...), Sys: sys}
}
