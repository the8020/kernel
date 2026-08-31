// Package services owns service Worker pools, scaling, and HTTP exposure.
package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/execution/coordinator"
	executionprofile "the8020/kernel/execution/profile"
	"the8020/kernel/execution/records"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/ports"
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
type Router interface {
	RegisterRoute(string, http.Handler) error
	UnregisterRoute(string)
}
type PortManager interface {
	ExposeHTTP(context.Context, ports.Request, http.Handler) (ports.Lease, error)
	AttachHTTP(context.Context, string, http.Handler) (ports.Lease, error)
	Close(string) error
}
type Policy struct {
	Strategy          model.GroupingStrategy
	Profile           model.RuntimeProfile
	Resources         model.ResourceLimits
	Lifecycle         model.LifecyclePolicy
	MinimumWorkers    int
	MaximumWorkers    int
	MaximumInFlight   int
	WorkerIdleTimeout time.Duration
	RecycleRequests   int
	ScaleDownCooldown time.Duration
	WorkspaceMounts   executionprofile.MountPolicy
	Logger            *slog.Logger
}
type Options struct {
	GroupKey           string
	Namespace          string
	MinimumWorkers     int
	MaximumWorkers     int
	MaximumInFlight    int
	PathPrefix         string
	Ephemeral          bool
	IdleTimeout        time.Duration
	Permissions        *supervisor.WorkerPermissions
	ReleaseID          string
	Workspace          string
	WorkspaceWritable  bool
	LogicalServiceID   string
	Generation         uint64
	CanonicalBasePath  string
	OpenAPI            supervisor.OpenAPIMetadata
	ValidateEntrypoint bool
	Instance           int
	DependencyMode     model.DependencyMode
	ExecutionMode      string
	TargetUtilization  float64
}
type ExposeOptions struct {
	PathPrefix        string
	HostPort          int
	AutomaticHostPort bool
	BindAddress       string
	Expiration        time.Time
}
type Record struct {
	ServiceID          string                       `json:"service_id"`
	Entrypoint         string                       `json:"entrypoint"`
	RuntimeGroupID     string                       `json:"runtime_group_id,omitempty"`
	SandboxID          string                       `json:"sandbox_id,omitempty"`
	SandboxIP          string                       `json:"sandbox_ip,omitempty"`
	WorkerIDs          []string                     `json:"worker_ids"`
	ReleaseID          string                       `json:"release_id"`
	State              string                       `json:"state"`
	MinimumWorkers     int                          `json:"minimum_workers"`
	MaximumWorkers     int                          `json:"maximum_workers"`
	MaximumInFlight    int                          `json:"maximum_in_flight"`
	PathPrefix         string                       `json:"path_prefix,omitempty"`
	PortLeaseID        string                       `json:"port_lease_id,omitempty"`
	HostPort           int                          `json:"host_port,omitempty"`
	Ephemeral          bool                         `json:"ephemeral"`
	IdleTimeout        time.Duration                `json:"idle_timeout"`
	StartedAt          time.Time                    `json:"started_at"`
	Failure            string                       `json:"failure,omitempty"`
	RuntimeUnavailable bool                         `json:"runtime_unavailable,omitempty"`
	Permissions        supervisor.WorkerPermissions `json:"permissions"`
	WorkerRequests     map[string]int               `json:"worker_requests,omitempty"`
	LogicalServiceID   string                       `json:"logical_service_id,omitempty"`
	Generation         uint64                       `json:"generation,omitempty"`
	CanonicalBasePath  string                       `json:"canonical_base_path,omitempty"`
	OpenAPI            supervisor.OpenAPIMetadata   `json:"openapi,omitempty"`
	ValidateEntrypoint bool                         `json:"validate_entrypoint,omitempty"`
	Instance           int                          `json:"instance"`
	ExecutionMode      string                       `json:"execution_mode,omitempty"`
	TargetUtilization  float64                      `json:"target_utilization,omitempty"`
	OccupiedSlots      int                          `json:"-"`
	CapacitySlots      int                          `json:"-"`
}

var ErrReplicaCapacity = errors.New("service replica has no target capacity")

var ErrInvalidServiceDefinition = errors.New("service definition was rejected by the runtime")

type invalidServiceDefinitionError struct{ cause error }

func (e *invalidServiceDefinitionError) Error() string { return e.cause.Error() }
func (e *invalidServiceDefinitionError) Unwrap() []error {
	return []error{ErrInvalidServiceDefinition, e.cause}
}

type ReplicaCapacityError struct {
	Occupied int
	Slots    int
	Reason   string
}

func (e *ReplicaCapacityError) Error() string {
	return fmt.Sprintf("%s: %d of %d execution slots occupied", e.Reason, e.Occupied, e.Slots)
}

func (e *ReplicaCapacityError) Unwrap() error { return ErrReplicaCapacity }

type Manager struct {
	mu          sync.Mutex
	coordinator GroupCoordinator
	workers     WorkerManager
	store       *records.Store
	router      Router
	ports       PortManager
	policy      Policy
	now         func() time.Time
	timers      map[string]*time.Timer
	underTarget map[string]time.Time
	logger      *slog.Logger
}

func New(groupCoordinator GroupCoordinator, workerManager WorkerManager, store *records.Store, router Router, portManager PortManager, policy Policy) (*Manager, error) {
	if groupCoordinator == nil || workerManager == nil || store == nil || router == nil {
		return nil, errors.New("coordinator, Worker manager, service store, and router are required")
	}
	if policy.Strategy == "" {
		policy.Strategy = model.GroupingOwner
	}
	if !policy.Strategy.Valid() {
		return nil, errors.New("valid service grouping strategy is required")
	}
	if policy.MinimumWorkers < 0 {
		policy.MinimumWorkers = 1
	}
	if policy.MaximumWorkers <= 0 {
		policy.MaximumWorkers = 8
	}
	if policy.MinimumWorkers > policy.MaximumWorkers {
		return nil, errors.New("service minimum Workers exceeds maximum")
	}
	if policy.MaximumInFlight <= 0 {
		policy.MaximumInFlight = 32
	}
	if policy.ScaleDownCooldown <= 0 {
		policy.ScaleDownCooldown = 30 * time.Second
	}
	return &Manager{coordinator: groupCoordinator, workers: workerManager, store: store, router: router, ports: portManager, policy: policy, now: func() time.Time { return time.Now().UTC() }, timers: map[string]*time.Timer{}, underTarget: map[string]time.Time{}, logger: policy.Logger}, nil
}

func (m *Manager) Start(ctx context.Context, serviceID, entrypoint string, options Options) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if serviceID == "" || entrypoint == "" {
		return Record{}, errors.New("service ID and entrypoint are required")
	}
	if options.ExecutionMode == "" {
		options.ExecutionMode = "stateless"
	}
	if options.ExecutionMode != "stateless" && options.ExecutionMode != "persistent" {
		return Record{}, errors.New("service execution mode must be stateless or persistent")
	}
	if options.TargetUtilization == 0 {
		options.TargetUtilization = 0.7
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
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		m.quarantineInvalidRecord(serviceID, loadErr)
	}
	minimum := options.MinimumWorkers
	if minimum == 0 && !options.Ephemeral {
		minimum = m.policy.MinimumWorkers
	}
	maximum := options.MaximumWorkers
	if maximum == 0 {
		maximum = m.policy.MaximumWorkers
	}
	maxInFlight := options.MaximumInFlight
	if maxInFlight == 0 {
		maxInFlight = m.policy.MaximumInFlight
	}
	if minimum < 0 || maximum < 1 || minimum > maximum || maxInFlight < 1 {
		return Record{}, errors.New("invalid service Worker limits")
	}
	if options.ReleaseID == "" {
		options.ReleaseID = "development"
	}
	idleTimeout := options.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = m.policy.WorkerIdleTimeout
	}
	record := Record{ServiceID: serviceID, LogicalServiceID: options.LogicalServiceID, Generation: options.Generation, CanonicalBasePath: options.CanonicalBasePath, OpenAPI: options.OpenAPI, ValidateEntrypoint: options.ValidateEntrypoint, Instance: options.Instance, Entrypoint: entrypoint, ReleaseID: options.ReleaseID, State: "STARTING", MinimumWorkers: minimum, MaximumWorkers: maximum, MaximumInFlight: maxInFlight, Ephemeral: options.Ephemeral, IdleTimeout: idleTimeout, StartedAt: m.now(), WorkerRequests: map[string]int{}, ExecutionMode: options.ExecutionMode, TargetUtilization: options.TargetUtilization}
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
	group, err := m.coordinator.Ensure(ctx, coordinator.Request{WorkloadType: model.WorkloadService, OwnerID: serviceID, ExecutionID: executionID, Namespace: options.Namespace, PlacementGroup: &placementGroup, ReplicaServiceID: record.LogicalServiceID, Strategy: m.policy.Strategy, Profile: profile, ResourceLimits: m.policy.Resources, Lifecycle: m.policy.Lifecycle})
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
	configureErr := m.workers.ConfigureService(ctx, record.RuntimeGroupID, serviceID, record.WorkerIDs, record.MaximumInFlight)
	if configureErr != nil {
		return m.failStart(record, configureErr)
	}
	record.State = "READY"
	if options.PathPrefix != "" {
		record.PathPrefix = canonicalPrefix(options.PathPrefix)
		if err := m.router.RegisterRoute(record.PathPrefix, m.handler(record)); err != nil {
			record.PathPrefix = ""
			return m.failStart(record, err)
		}
	}
	if err := m.store.Save(serviceID, record); err != nil {
		if record.PathPrefix != "" {
			m.router.UnregisterRoute(record.PathPrefix)
			record.PathPrefix = ""
		}
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
		return record, err
	}
	return m.scaleRecordLocked(ctx, record, count, live)
}

func (m *Manager) scaleRecordLocked(ctx context.Context, record Record, count int, live []workers.Record) (Record, error) {
	record, cleanupErr, err := m.reconcileWorkerSetLocked(ctx, record, live)
	if err != nil {
		return record, err
	}
	desiredState := "READY"
	if count == 0 && record.Ephemeral {
		desiredState = "IDLE"
	}
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
	for _, id := range removed {
		if err := m.workers.StopInGroup(ctx, record.RuntimeGroupID, id, false); err != nil {
			return record, errors.Join(cleanupErr, err)
		}
	}
	if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, record.ServiceID, desiredWorkerIDs, record.MaximumInFlight); err != nil {
		for _, id := range record.WorkerIDs {
			if !contains(previousWorkerIDs, id) {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, id, true)
			}
		}
		return record, errors.Join(cleanupErr, err)
	}
	record.WorkerIDs = desiredWorkerIDs
	if len(removed) > 0 {
		for _, id := range removed {
			delete(record.WorkerRequests, id)
		}
		if err := m.store.Save(record.ServiceID, record); err != nil {
			return record, errors.Join(cleanupErr, err)
		}
	}
	record.State = desiredState
	record.Failure = ""
	if count > 0 {
		m.stopIdleTimerLocked(record.ServiceID)
	}
	if err := m.store.Save(record.ServiceID, record); err != nil {
		return record, errors.Join(cleanupErr, err)
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
		delete(record.WorkerRequests, workerID)
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
		if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, record.ServiceID, ready, record.MaximumInFlight); err != nil {
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

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, timer := range m.timers {
		timer.Stop()
		delete(m.timers, id)
	}
	return nil
}

func (m *Manager) Expose(ctx context.Context, serviceID string, options ExposeOptions) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if record.PathPrefix != "" || record.PortLeaseID != "" {
		return record, errors.New("service is already exposed")
	}
	prefix := options.PathPrefix
	if prefix == "" {
		prefix = "/services/" + serviceID
	}
	prefix = canonicalPrefix(prefix)
	handler := m.handler(record)
	if err := m.router.RegisterRoute(prefix, handler); err != nil {
		return record, err
	}
	record.PathPrefix = prefix
	if options.AutomaticHostPort || options.HostPort != 0 {
		if m.ports == nil {
			m.router.UnregisterRoute(prefix)
			return record, errors.New("host-port manager is unavailable")
		}
		lease, leaseErr := m.ports.ExposeHTTP(ctx, ports.Request{SandboxID: record.SandboxID, OwnerID: record.ServiceID, SandboxIP: record.SandboxIP, InternalPort: 8000, BindAddress: options.BindAddress, HostPort: options.HostPort, Protocol: "http", Purpose: "service", ExpiresAt: options.Expiration}, handler)
		if leaseErr != nil {
			m.router.UnregisterRoute(prefix)
			return record, leaseErr
		}
		record.PortLeaseID, record.HostPort = lease.LeaseID, lease.HostPort
	}
	if err := m.store.Save(serviceID, record); err != nil {
		m.router.UnregisterRoute(prefix)
		if record.PortLeaseID != "" {
			_ = m.ports.Close(record.PortLeaseID)
		}
		return record, err
	}
	return record, nil
}

func (m *Manager) Unexpose(serviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return err
	}
	if record.PathPrefix != "" {
		m.router.UnregisterRoute(record.PathPrefix)
	}
	if record.PortLeaseID != "" && m.ports != nil {
		if err := m.ports.Close(record.PortLeaseID); err != nil {
			return err
		}
	}
	record.PathPrefix = ""
	record.PortLeaseID = ""
	record.HostPort = 0
	return m.store.Save(serviceID, record)
}
func (m *Manager) Stop(ctx context.Context, serviceID string) error {
	if err := m.Unexpose(serviceID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopIdleTimerLocked(serviceID)
	record, err := m.inspect(serviceID)
	if err != nil {
		return err
	}
	if record.State == "STOPPED" && len(record.WorkerIDs) == 0 {
		return m.releaseReplica(ctx, record)
	}
	if record.State == "FAILED" && record.RuntimeUnavailable {
		record.WorkerIDs = nil
		record.WorkerRequests = map[string]int{}
		record.State = "STOPPED"
		if err := m.store.Save(serviceID, record); err != nil {
			return err
		}
		return m.releaseReplica(ctx, record)
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Runtime records are recoverable indexes, not authority over a
		// sandbox. Startup may already have removed the inherited group, so
		// retire this pool without requiring its vanished Workers to answer.
		record.WorkerIDs = nil
		record.WorkerRequests = map[string]int{}
		record.State = "STOPPED"
		record.Failure = ""
		if err := m.store.Save(serviceID, record); err != nil {
			return err
		}
		return m.releaseReplica(ctx, record)
	}
	liveIDs := make(map[string]bool, len(live))
	for _, item := range live {
		liveIDs[item.Worker.WorkerID] = true
	}
	remaining := record.WorkerIDs[:0]
	for _, id := range record.WorkerIDs {
		if liveIDs[id] {
			remaining = append(remaining, id)
		} else {
			delete(record.WorkerRequests, id)
		}
	}
	record.WorkerIDs = append([]string(nil), remaining...)
	if err := m.workers.ConfigureService(ctx, record.RuntimeGroupID, serviceID, nil, record.MaximumInFlight); err != nil {
		return err
	}
	record.State = "DRAINING"
	if err := m.store.Save(serviceID, record); err != nil {
		return err
	}
	var joined error
	remaining = append([]string(nil), record.WorkerIDs...)
	for _, id := range append([]string(nil), record.WorkerIDs...) {
		if stopErr := m.workers.StopInGroup(ctx, record.RuntimeGroupID, id, false); stopErr != nil {
			joined = errors.Join(joined, fmt.Errorf("stop Worker %s: %w", id, stopErr))
			continue
		}
		remaining = remove(remaining, id)
		record.WorkerIDs = append([]string(nil), remaining...)
		delete(record.WorkerRequests, id)
		if saveErr := m.store.Save(serviceID, record); saveErr != nil {
			joined = errors.Join(joined, saveErr)
			break
		}
	}
	if joined != nil {
		record.Failure = joined.Error()
		_ = m.store.Save(serviceID, record)
		return joined
	}
	record.WorkerIDs = nil
	record.State = "STOPPED"
	record.Failure = ""
	if err := m.store.Save(serviceID, record); err != nil {
		return err
	}
	return m.releaseReplica(ctx, record)
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
	m.stopIdleTimerLocked(serviceID)
	return m.store.Delete(serviceID)
}

func (m *Manager) releaseReplica(ctx context.Context, record Record) error {
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
	return m.workers.ServiceOpenAPI(ctx, record.RuntimeGroupID, record.ServiceID)
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
		m.stopIdleTimerLocked(record.ServiceID)
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
	m.stopIdleTimerLocked(record.ServiceID)
	if record.PathPrefix != "" {
		m.router.UnregisterRoute(record.PathPrefix)
	}
	record.WorkerIDs = nil
	record.WorkerRequests = map[string]int{}
	record.PathPrefix = ""
	record.PortLeaseID = ""
	record.HostPort = 0
	record.State = "STOPPED"
	record.Failure = reason
	record.RuntimeUnavailable = true
	return m.store.Save(record.ServiceID, record)
}

// Restore re-registers persisted routes and replaces restored raw service
// listeners with Go-owned HTTP handlers after kernel restart.
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
		handler := m.handler(record)
		if record.PathPrefix != "" {
			if err := m.router.RegisterRoute(record.PathPrefix, handler); err != nil {
				m.persistRestoreFailure(record, fmt.Errorf("restore route: %w", err))
				continue
			}
		}
		if record.PortLeaseID == "" {
			continue
		}
		if m.ports == nil {
			m.router.UnregisterRoute(record.PathPrefix)
			m.persistRestoreFailure(record, errors.New("restore host port: manager is unavailable"))
			continue
		}
		lease, err := m.ports.AttachHTTP(ctx, record.PortLeaseID, handler)
		if err != nil {
			m.router.UnregisterRoute(record.PathPrefix)
			m.persistRestoreFailure(record, fmt.Errorf("restore host port: %w", err))
			continue
		}
		if lease.LeaseID != record.PortLeaseID || lease.HostPort != record.HostPort {
			m.router.UnregisterRoute(record.PathPrefix)
			m.persistRestoreFailure(record, errors.New("restore host port changed durable lease identity"))
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
	if record.WorkerRequests == nil {
		record.WorkerRequests = map[string]int{}
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
func (m *Manager) handler(record Record) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		response, err := m.Dispatch(request.Context(), record.ServiceID, request)
		if err != nil {
			http.Error(writer, "service unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				writer.Header().Add(key, value)
			}
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
	})
}

// Dispatch sends one streaming request through an existing service pool.
func (m *Manager) Dispatch(ctx context.Context, serviceID string, request *http.Request) (*http.Response, error) {
	var current Record
	var err error
	if request.Header.Get("X-80-20-Internal-Persistent-Existing") == "true" {
		current, err = m.Inspect(serviceID)
	} else {
		current, err = m.EnsureCapacity(ctx, serviceID)
		var capacity *ReplicaCapacityError
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
	response.Body = &completionBody{ReadCloser: response.Body, complete: func() { m.completeRequest(serviceID, workerID) }}
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
		return errors.New("service instance is unavailable")
	}
	return m.workers.ProxyServiceWebSocket(ctx, record.RuntimeGroupID, record.ServiceID, writer, request)
}

// EnsureCapacity applies the per-replica portion of the scaling order. It
// grows Workers first and reports ErrReplicaCapacity only when another replica
// is required to preserve target headroom.
func (m *Manager) EnsureCapacity(ctx context.Context, serviceID string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if record.TargetUtilization <= 0 || record.TargetUtilization > 1 {
		record.TargetUtilization = 0.7
	}
	if record.MaximumWorkers <= 0 {
		record.MaximumWorkers = m.policy.MaximumWorkers
	}
	if record.MaximumInFlight <= 0 {
		record.MaximumInFlight = m.policy.MaximumInFlight
	}
	if record.MinimumWorkers <= 0 && !record.Ephemeral {
		record.MinimumWorkers = m.policy.MinimumWorkers
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		return record, err
	}
	if workerSetNeedsReconciliation(record, live) {
		target := len(record.WorkerIDs)
		record, err = m.scaleRecordLocked(ctx, record, target, live)
		if err != nil {
			return record, err
		}
		live, err = m.workers.List(ctx, record.RuntimeGroupID)
		if err != nil {
			return record, err
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
	if healthy == 0 {
		if len(record.WorkerIDs) < record.MaximumWorkers {
			result, scaleErr := m.scaleRecordLocked(ctx, record, max(record.MinimumWorkers, 1), live)
			return withCapacitySnapshot(result, occupied), scaleErr
		}
		return record, &ReplicaCapacityError{Occupied: 0, Slots: 0, Reason: "replica has no healthy Workers"}
	}
	requiredWorkers := int(math.Ceil(float64(occupied+1) / (float64(record.MaximumInFlight) * record.TargetUtilization)))
	requiredWorkers = max(requiredWorkers, record.MinimumWorkers)
	if requiredWorkers > record.MaximumWorkers {
		if len(record.WorkerIDs) < record.MaximumWorkers {
			result, scaleErr := m.scaleRecordLocked(ctx, record, record.MaximumWorkers, live)
			return withCapacitySnapshot(result, occupied), scaleErr
		}
		return record, &ReplicaCapacityError{Occupied: occupied, Slots: healthy * record.MaximumInFlight, Reason: fmt.Sprintf("target utilization requires %d Workers but replica maximum is %d", requiredWorkers, record.MaximumWorkers)}
	}
	if requiredWorkers > len(record.WorkerIDs) {
		result, scaleErr := m.scaleRecordLocked(ctx, record, requiredWorkers, live)
		return withCapacitySnapshot(result, occupied), scaleErr
	}
	return withCapacitySnapshot(record, occupied), nil
}

// ReconcileCapacity repairs a replica and applies kernel-owned scale-down
// hysteresis. Only idle Workers above the declared minimum are removable.
func (m *Manager) ReconcileCapacity(ctx context.Context, serviceID string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil {
		return record, err
	}
	if record.TargetUtilization <= 0 || record.TargetUtilization > 1 {
		record.TargetUtilization = 0.7
	}
	if record.MaximumInFlight <= 0 {
		record.MaximumInFlight = m.policy.MaximumInFlight
	}
	if record.MaximumWorkers <= 0 {
		record.MaximumWorkers = m.policy.MaximumWorkers
	}
	if record.MinimumWorkers <= 0 && !record.Ephemeral {
		record.MinimumWorkers = m.policy.MinimumWorkers
	}
	live, err := m.workers.List(ctx, record.RuntimeGroupID)
	if err != nil {
		return record, err
	}
	if workerSetNeedsReconciliation(record, live) {
		record, err = m.scaleRecordLocked(ctx, record, len(record.WorkerIDs), live)
		if err != nil {
			return record, err
		}
		live, err = m.workers.List(ctx, record.RuntimeGroupID)
		if err != nil {
			return record, err
		}
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
		utilization = float64(occupied) / float64(healthy*record.MaximumInFlight)
	}
	if len(record.WorkerIDs) <= record.MinimumWorkers || utilization >= record.TargetUtilization*0.5 {
		delete(m.underTarget, serviceID)
		return withCapacitySnapshot(record, occupied), nil
	}
	since, exists := m.underTarget[serviceID]
	if !exists {
		m.underTarget[serviceID] = m.now()
		return withCapacitySnapshot(record, occupied), nil
	}
	if m.now().Sub(since) < m.policy.ScaleDownCooldown {
		return withCapacitySnapshot(record, occupied), nil
	}
	result, err := m.scaleRecordLocked(ctx, record, len(record.WorkerIDs)-1, live)
	if err == nil {
		m.underTarget[serviceID] = m.now()
	}
	return withCapacitySnapshot(result, occupied), err
}

func withCapacitySnapshot(record Record, occupied int) Record {
	record.OccupiedSlots = occupied
	record.CapacitySlots = len(record.WorkerIDs) * record.MaximumInFlight
	return record
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

func (m *Manager) completeRequest(serviceID, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.inspect(serviceID)
	if err != nil || (record.State != "READY" && record.State != "IDLE") {
		return
	}
	if record.WorkerRequests == nil {
		record.WorkerRequests = map[string]int{}
	}
	if workerID != "" {
		record.WorkerRequests[workerID]++
	}
	if workerID != "" && m.policy.RecycleRequests > 0 && record.WorkerRequests[workerID] >= m.policy.RecycleRequests {
		permissions := record.Permissions
		if len(permissions.Read)+len(permissions.Write)+len(permissions.Net)+len(permissions.Import)+len(permissions.Env)+len(permissions.Sys) == 0 {
			permissions = permissionsFor(m.policy.Profile.Permissions)
		}
		replacement, startErr := m.startWorker(context.Background(), record, permissions)
		if startErr == nil {
			next := make([]string, 0, len(record.WorkerIDs))
			for _, id := range record.WorkerIDs {
				if id != workerID {
					next = append(next, id)
				}
			}
			next = append(next, replacement)
			sort.Strings(next)
			if configureErr := m.workers.ConfigureService(context.Background(), record.RuntimeGroupID, serviceID, next, record.MaximumInFlight); configureErr == nil {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, workerID, false)
				record.WorkerIDs = next
				delete(record.WorkerRequests, workerID)
				record.WorkerRequests[replacement] = 0
			} else {
				_ = m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, replacement, true)
			}
		}
	}
	_ = m.store.Save(serviceID, record)
	if record.Ephemeral && record.MinimumWorkers == 0 && record.IdleTimeout > 0 {
		m.stopIdleTimerLocked(serviceID)
		m.timers[serviceID] = time.AfterFunc(record.IdleTimeout, func() { m.scaleIdle(serviceID) })
	}
}

func (m *Manager) scaleIdle(serviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.timers, serviceID)
	record, err := m.inspect(serviceID)
	if err != nil || !record.Ephemeral || record.MinimumWorkers != 0 {
		return
	}
	live, err := m.workers.List(context.Background(), record.RuntimeGroupID)
	if err != nil {
		return
	}
	for _, item := range live {
		if item.Worker.InFlight > 0 {
			m.timers[serviceID] = time.AfterFunc(record.IdleTimeout, func() { m.scaleIdle(serviceID) })
			return
		}
	}
	_, _ = m.scaleLocked(context.Background(), serviceID, 0)
}

func (m *Manager) stopIdleTimerLocked(serviceID string) {
	if timer := m.timers[serviceID]; timer != nil {
		timer.Stop()
		delete(m.timers, serviceID)
	}
}

type completionBody struct {
	io.ReadCloser
	once     sync.Once
	complete func()
}

func (b *completionBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if err == io.EOF {
		b.once.Do(b.complete)
	}
	return count, err
}

func (b *completionBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.complete)
	return err
}
func (m *Manager) fail(record Record, cause error) (Record, error) {
	record.State = "FAILED"
	record.Failure = cause.Error()
	record.RuntimeUnavailable = false
	_ = m.store.Save(record.ServiceID, record)
	return record, cause
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
		cleanupErr = errors.Join(cleanupErr, m.releaseReplica(context.Background(), record))
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
func canonicalPrefix(value string) string {
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		return "/"
	}
	return value
}
func permissionsFor(value model.Permissions) supervisor.WorkerPermissions {
	sys := []string(nil)
	if value.SystemInfo {
		sys = []string{"hostname", "osRelease"}
	}
	return supervisor.WorkerPermissions{Read: append([]string(nil), value.ReadPaths...), Write: append([]string(nil), value.WritePaths...), Net: append([]string(nil), value.NetworkHosts...), Import: append([]string(nil), value.ImportHosts...), Env: append([]string(nil), value.Environment...), Sys: sys}
}
