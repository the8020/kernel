// Package manager coordinates persisted sandbox lifecycle and observed runtime state.
package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/model"
	sandboxnetwork "the8020/kernel/sandbox/network"
	"the8020/kernel/sandbox/resources"
	"the8020/kernel/sandbox/state"
)

type Network interface {
	Allocate(context.Context, string, string, model.NetworkConfiguration) (sandboxnetwork.Allocation, error)
	Check(context.Context, string) error
	Release(context.Context, string) error
}

type Supervisor interface {
	Status(context.Context, model.SandboxSpec) (supervisor.Status, error)
	Workers(context.Context, model.SandboxSpec) ([]supervisor.WorkerStatus, error)
	Drain(context.Context, model.SandboxSpec) error
}

type PortLeases interface {
	CloseForSandbox(string) error
}

type Config struct {
	InstanceUUID     string
	CgroupRoot       string
	StartupTimeout   time.Duration
	ProbeInterval    time.Duration
	StopGrace        time.Duration
	Store            *state.Store
	Backend          backend.Backend
	Network          Network
	Supervisor       Supervisor
	Ports            PortLeases
	History          *history.Store
	HistoryRetention time.Duration
	NodeLimits       NodeLimits
	Now              func() time.Time
}

// NodeLimits are kernel-owned admission budgets for all live sandboxes on one
// application-server node. Zero leaves the corresponding resource unlimited.
type NodeLimits struct {
	MemoryBytes           int64 `json:"memory_bytes"`
	CPUMillicores         int64 `json:"cpu_millicores"`
	TemporaryStorageBytes int64 `json:"temporary_storage_bytes"`
	MaximumSandboxes      int   `json:"maximum_sandboxes"`
	MaximumWorkers        int   `json:"maximum_workers"`
}

type NodeCapacity struct {
	Limits                NodeLimits `json:"limits"`
	SandboxCount          int        `json:"sandbox_count"`
	MemoryReservedBytes   int64      `json:"memory_reserved_bytes"`
	CPUReservedMillicores int64      `json:"cpu_reserved_millicores"`
	TemporaryStorageBytes int64      `json:"temporary_storage_reserved_bytes"`
}

type Manager struct {
	mu                 sync.Mutex
	instanceUUID       string
	cgroupRoot         string
	startupTimeout     time.Duration
	probeInterval      time.Duration
	stopGrace          time.Duration
	store              *state.Store
	backend            backend.Backend
	network            Network
	supervisor         Supervisor
	ports              PortLeases
	history            *history.Store
	historyRetention   time.Duration
	nodeLimits         NodeLimits
	reservedSandboxIDs map[string]bool
	now                func() time.Time
}

type Inspection struct {
	Spec    model.SandboxSpec         `json:"spec"`
	Status  model.SandboxStatus       `json:"status"`
	Workers []supervisor.WorkerStatus `json:"workers"`
}

type ReconcileReport struct {
	Restored       int             `json:"restored"`
	Missing        []string        `json:"missing"`
	Failed         []string        `json:"failed"`
	OrphansDeleted []string        `json:"orphans_deleted"`
	Terminated     []HealthFailure `json:"terminated,omitempty"`
}

type HealthFailure struct {
	RuntimeGroupID string `json:"runtime_group_id"`
	SandboxID      string `json:"sandbox_id"`
	Reason         string `json:"reason"`
	OOM            bool   `json:"oom"`
}

type HealthReport struct {
	Checked  int             `json:"checked"`
	Failures []HealthFailure `json:"failures,omitempty"`
}

type ShutdownPolicy string

const (
	ShutdownDestroy ShutdownPolicy = "destroy"
	ShutdownLeave   ShutdownPolicy = "leave"
)

type StartupPolicy string

const (
	StartupReconcile StartupPolicy = "reconcile"
	StartupDestroy   StartupPolicy = "destroy"
)

func New(config Config) (*Manager, error) {
	if config.InstanceUUID == "" || config.Store == nil || config.Backend == nil || config.Network == nil || config.Supervisor == nil || config.Ports == nil || config.History == nil {
		return nil, errors.New("instance UUID, state, backend, network, supervisor, port leases, and history are required")
	}
	if config.CgroupRoot == "" {
		config.CgroupRoot = "/sys/fs/cgroup"
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = 30 * time.Second
	}
	if config.ProbeInterval <= 0 {
		config.ProbeInterval = 200 * time.Millisecond
	}
	if config.StopGrace <= 0 {
		config.StopGrace = 10 * time.Second
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.HistoryRetention <= 0 {
		config.HistoryRetention = history.DefaultRetention
	}
	return &Manager{instanceUUID: config.InstanceUUID, cgroupRoot: config.CgroupRoot, startupTimeout: config.StartupTimeout, probeInterval: config.ProbeInterval, stopGrace: config.StopGrace, store: config.Store, backend: config.Backend, network: config.Network, supervisor: config.Supervisor, ports: config.Ports, history: config.History, historyRetention: config.HistoryRetention, nodeLimits: config.NodeLimits, reservedSandboxIDs: map[string]bool{}, now: config.Now}, nil
}

// NewSandboxID reserves one short ID after checking both the live catalog and
// the direct retained-history marker namespace.
func (m *Manager) NewSandboxID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for attempts := 0; attempts < 128; attempts++ {
		candidate, err := model.NewSandboxID()
		if err != nil {
			return "", err
		}
		if m.reservedSandboxIDs[candidate] {
			continue
		}
		if _, _, err := m.find(candidate); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		retained, err := m.history.ContainsSandboxID(candidate)
		if err != nil {
			return "", err
		}
		if retained {
			continue
		}
		m.reservedSandboxIDs[candidate] = true
		return candidate, nil
	}
	return "", errors.New("cannot allocate unique sandbox ID")
}

// ReleaseSandboxID drops an unused allocation reservation. Create also releases
// the reservation after accepting or rejecting the specification.
func (m *Manager) ReleaseSandboxID(sandboxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reservedSandboxIDs, sandboxID)
}

func (m *Manager) ListHistory(limit int, before string) (history.Page, error) {
	return m.history.List(limit, before)
}

func (m *Manager) InspectHistory(historyID string) (history.Inspection, error) {
	return m.history.Inspect(historyID)
}

func (m *Manager) CleanupHistory() (int, error) {
	return m.history.Cleanup(m.historyRetention)
}

func (m *Manager) Create(ctx context.Context, spec model.SandboxSpec) (Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer delete(m.reservedSandboxIDs, spec.SandboxID)
	if err := spec.Validate(); err != nil {
		return Inspection{}, err
	}
	if len(spec.InternalToken) < 16 {
		return Inspection{}, errors.New("high-entropy sandbox internal token is required")
	}
	if existing, _, err := m.find(spec.SandboxID); err == nil {
		return Inspection{}, fmt.Errorf("sandbox ID %s already belongs to runtime group %s", spec.SandboxID, existing.RuntimeGroupID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Inspection{}, err
	}
	retained, err := m.history.ContainsSandboxID(spec.SandboxID)
	if err != nil {
		return Inspection{}, err
	}
	if retained {
		return Inspection{}, fmt.Errorf("sandbox ID %s is retained in history", spec.SandboxID)
	}
	if _, _, err := m.store.Load(spec.RuntimeGroupID); err == nil {
		return Inspection{}, fmt.Errorf("runtime group %s already exists", spec.RuntimeGroupID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Inspection{}, err
	}
	if err := m.admitSandboxLocked(spec); err != nil {
		return Inspection{}, err
	}
	status := model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateCreating, CurrentOwners: append([]string(nil), spec.OwnerIDs...)}
	if err := m.store.SaveSpec(spec); err != nil {
		return Inspection{}, err
	}
	if err := m.store.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		return Inspection{}, err
	}
	allocation, err := m.network.Allocate(ctx, spec.RuntimeGroupID, spec.SandboxID, spec.Network)
	if err != nil {
		return Inspection{}, m.failCreate(spec, fmt.Errorf("allocate sandbox network: %w", err))
	}
	spec.Network.NamespacePath = allocation.NamespacePath
	spec.Network.NetworkName = allocation.NetworkName
	spec.Network.SupervisorPort = allocation.SupervisorPort
	spec.Network.InspectorPort = allocation.InspectorPort
	if len(allocation.IPs) > 0 {
		spec.Network.SandboxIP = allocation.IPs[0]
	}
	if err := m.store.SaveSpec(spec); err != nil {
		return Inspection{}, m.failCreate(spec, err)
	}
	if _, err := m.store.Transition(spec.RuntimeGroupID, model.StateStarting, nil); err != nil {
		return Inspection{}, m.failCreate(spec, err)
	}
	observation, err := m.backend.Create(ctx, spec)
	if err != nil {
		return Inspection{}, m.failCreate(spec, fmt.Errorf("create sandbox backend: %w", err))
	}
	loadedSpec, current, loadErr := m.store.Load(spec.RuntimeGroupID)
	if loadErr != nil {
		return Inspection{}, loadErr
	}
	current.ContainerID, current.TaskPID, current.SandboxIP, current.StartedAt = observation.ContainerID, observation.TaskPID, loadedSpec.Network.SandboxIP, m.now()
	if err := m.store.SaveStatus(spec.RuntimeGroupID, current); err != nil {
		return Inspection{}, m.failCreate(spec, err)
	}
	supervisorStatus, err := m.waitReady(ctx, spec)
	if err != nil {
		return Inspection{}, m.failCreate(spec, err)
	}
	ready, err := m.store.Transition(spec.RuntimeGroupID, model.StateReady, func(value *model.SandboxStatus) {
		value.SupervisorHealthy = true
		value.SupervisorVersion = supervisorStatus.SupervisorVersion
		value.DenoVersion = supervisorStatus.DenoVersion
		value.WorkerCount = supervisorStatus.WorkerCount
		value.LastHeartbeat = m.now()
	})
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Spec: spec, Status: ready}, nil
}

func (m *Manager) Capacity() (NodeCapacity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityLocked()
}

func (m *Manager) capacityLocked() (NodeCapacity, error) {
	result := NodeCapacity{Limits: m.nodeLimits}
	ids, err := m.store.List()
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		spec, _, loadErr := m.store.Load(id)
		if loadErr != nil {
			return result, loadErr
		}
		result.SandboxCount++
		result.MemoryReservedBytes += spec.ResourceLimits.MemoryMaximum
		result.CPUReservedMillicores += sandboxCPUMillicores(spec.ResourceLimits)
		result.TemporaryStorageBytes += spec.ResourceLimits.TmpfsMaximum
	}
	return result, nil
}

func (m *Manager) admitSandboxLocked(spec model.SandboxSpec) error {
	capacity, err := m.capacityLocked()
	if err != nil {
		return err
	}
	nextSandboxes := capacity.SandboxCount + 1
	nextMemory := capacity.MemoryReservedBytes + spec.ResourceLimits.MemoryMaximum
	nextCPU := capacity.CPUReservedMillicores + sandboxCPUMillicores(spec.ResourceLimits)
	nextStorage := capacity.TemporaryStorageBytes + spec.ResourceLimits.TmpfsMaximum
	if limit := m.nodeLimits.MaximumSandboxes; limit > 0 && nextSandboxes > limit {
		return fmt.Errorf("node sandbox capacity exhausted: %d of %d sandboxes are allocated", capacity.SandboxCount, limit)
	}
	if limit := m.nodeLimits.MemoryBytes; limit > 0 && nextMemory > limit {
		return fmt.Errorf("node memory capacity exhausted: %d of %d bytes are reserved", capacity.MemoryReservedBytes, limit)
	}
	if limit := m.nodeLimits.CPUMillicores; limit > 0 && nextCPU > limit {
		return fmt.Errorf("node CPU capacity exhausted: %d of %d millicores are reserved", capacity.CPUReservedMillicores, limit)
	}
	if limit := m.nodeLimits.TemporaryStorageBytes; limit > 0 && nextStorage > limit {
		return fmt.Errorf("node temporary storage capacity exhausted: %d of %d bytes are reserved", capacity.TemporaryStorageBytes, limit)
	}
	return nil
}

func sandboxCPUMillicores(limits model.ResourceLimits) int64 {
	if limits.CPUQuotaMicros <= 0 || limits.CPUPeriodMicros <= 0 {
		return 0
	}
	return (limits.CPUQuotaMicros*1000 + limits.CPUPeriodMicros - 1) / limits.CPUPeriodMicros
}

// AssignWarm converts one healthy, unused warm runtime group into an assigned
// group. An assigned supervisor is never eligible to return to the warm pool.
func (m *Manager) AssignWarm(ctx context.Context, runtimeGroupID, groupKey, ownerID string) (Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtimeGroupID == "" || groupKey == "" || ownerID == "" {
		return Inspection{}, errors.New("runtime-group ID, group key, and owner ID are required")
	}
	priorSpec, priorStatus, err := m.store.Load(runtimeGroupID)
	if err != nil {
		return Inspection{}, err
	}
	if !priorSpec.Lifecycle.Warm || priorStatus.ObservedState != model.StateReady || !priorStatus.SupervisorHealthy {
		return Inspection{}, errors.New("runtime group is not a healthy ready warm group")
	}
	live, err := m.supervisor.Status(ctx, priorSpec)
	if err != nil {
		return Inspection{}, fmt.Errorf("verify warm supervisor health: %w", err)
	}
	if live.Draining {
		return Inspection{}, errors.New("warm supervisor is draining")
	}
	workers, err := m.supervisor.Workers(ctx, priorSpec)
	if err != nil {
		return Inspection{}, fmt.Errorf("verify warm supervisor Workers: %w", err)
	}
	if live.WorkerCount != 0 || len(workers) != 0 {
		return Inspection{}, errors.New("warm runtime group is not clean")
	}
	assigned := priorSpec
	assigned.GroupKey = groupKey
	assigned.OwnerIDs = []string{ownerID}
	assigned.Lifecycle.Warm = false
	if assigned.Labels == nil {
		assigned.Labels = map[string]string{}
	}
	delete(assigned.Labels, "the8020.warm")
	assigned.Labels["the8020.owner"] = ownerID
	assigned.Labels["the8020.group_key"] = groupKey
	assigned.Labels["the8020.assigned_at"] = m.now().Format(time.RFC3339Nano)
	if err := assigned.Validate(); err != nil {
		return Inspection{}, err
	}
	assignedStatus := priorStatus
	assignedStatus.CurrentOwners = []string{ownerID}
	if err := m.store.SaveSpec(assigned); err != nil {
		return Inspection{}, err
	}
	if err := m.store.SaveStatus(runtimeGroupID, assignedStatus); err != nil {
		_ = m.store.SaveSpec(priorSpec)
		return Inspection{}, err
	}
	labels := map[string]string{"the8020.owner": ownerID, "the8020.owners": ownerID, "the8020.group_key": groupKey, "the8020.assigned_at": assigned.Labels["the8020.assigned_at"]}
	if err := m.backend.UpdateLabels(ctx, assigned.SandboxID, labels); err != nil {
		rollbackErr := errors.Join(m.store.SaveSpec(priorSpec), m.store.SaveStatus(runtimeGroupID, priorStatus))
		return Inspection{}, errors.Join(fmt.Errorf("assign warm sandbox labels: %w", err), rollbackErr)
	}
	return Inspection{Spec: assigned, Status: assignedStatus}, nil
}

// AddOwner records another workload owner on an existing shared runtime group.
func (m *Manager) AddOwner(ctx context.Context, runtimeGroupID, ownerID string, logicalServiceID ...string) (Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtimeGroupID == "" || strings.TrimSpace(ownerID) == "" {
		return Inspection{}, errors.New("runtime-group ID and owner ID are required")
	}
	priorSpec, priorStatus, err := m.store.Load(runtimeGroupID)
	if err != nil {
		return Inspection{}, err
	}
	if priorSpec.Lifecycle.Warm {
		return Inspection{}, errors.New("cannot add an owner to an unassigned warm runtime group")
	}
	if priorStatus.ObservedState != model.StateReady && priorStatus.ObservedState != model.StateActive {
		return Inspection{}, errors.New("cannot add an owner to an unavailable runtime group")
	}
	for _, existing := range priorSpec.OwnerIDs {
		if existing == ownerID {
			return Inspection{Spec: priorSpec, Status: priorStatus}, nil
		}
	}
	updatedSpec, updatedStatus := priorSpec, priorStatus
	updatedSpec.OwnerIDs = append(append([]string(nil), priorSpec.OwnerIDs...), ownerID)
	updatedStatus.CurrentOwners = append(append([]string(nil), priorStatus.CurrentOwners...), ownerID)
	sort.Strings(updatedSpec.OwnerIDs)
	sort.Strings(updatedStatus.CurrentOwners)
	if len(logicalServiceID) > 0 && logicalServiceID[0] != "" {
		if slices.Contains(priorSpec.ServiceIDs, logicalServiceID[0]) {
			return Inspection{}, fmt.Errorf("service %q already has a sandbox allocation in runtime group %s", logicalServiceID[0], runtimeGroupID)
		}
		updatedSpec.ServiceIDs = append(append([]string(nil), priorSpec.ServiceIDs...), logicalServiceID[0])
		sort.Strings(updatedSpec.ServiceIDs)
	}
	if err := updatedSpec.Validate(); err != nil {
		return Inspection{}, err
	}
	if err := m.store.SaveSpec(updatedSpec); err != nil {
		return Inspection{}, err
	}
	if err := m.store.SaveStatus(runtimeGroupID, updatedStatus); err != nil {
		return Inspection{}, errors.Join(err, m.store.SaveSpec(priorSpec))
	}
	if err := m.backend.UpdateLabels(ctx, updatedSpec.SandboxID, map[string]string{"the8020.owners": strings.Join(updatedSpec.OwnerIDs, ","), "the8020.services": strings.Join(updatedSpec.ServiceIDs, ",")}); err != nil {
		rollbackErr := errors.Join(m.store.SaveSpec(priorSpec), m.store.SaveStatus(runtimeGroupID, priorStatus))
		return Inspection{}, errors.Join(fmt.Errorf("update runtime-group owner labels: %w", err), rollbackErr)
	}
	return Inspection{Spec: updatedSpec, Status: updatedStatus}, nil
}

// RemoveOwner releases one workload from a shared runtime group. The sandbox
// is deleted when the final owner leaves; otherwise the remaining ownership
// and service-placement indexes are updated atomically before returning.
func (m *Manager) RemoveOwner(ctx context.Context, runtimeGroupID, ownerID, logicalServiceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtimeGroupID == "" || strings.TrimSpace(ownerID) == "" {
		return false, errors.New("runtime-group ID and owner ID are required")
	}
	spec, status, err := m.store.Load(runtimeGroupID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !slices.Contains(spec.OwnerIDs, ownerID) {
		return len(spec.OwnerIDs) == 0, nil
	}
	updatedSpec, updatedStatus := spec, status
	updatedSpec.OwnerIDs = removeString(updatedSpec.OwnerIDs, ownerID)
	updatedStatus.CurrentOwners = removeString(updatedStatus.CurrentOwners, ownerID)
	if logicalServiceID != "" {
		updatedSpec.ServiceIDs = removeString(updatedSpec.ServiceIDs, logicalServiceID)
	}
	if len(updatedSpec.OwnerIDs) == 0 {
		return true, m.deleteLocked(ctx, spec.SandboxID)
	}
	if err := updatedSpec.Validate(); err != nil {
		return false, err
	}
	if err := m.store.SaveSpec(updatedSpec); err != nil {
		return false, err
	}
	if err := m.store.SaveStatus(runtimeGroupID, updatedStatus); err != nil {
		return false, errors.Join(err, m.store.SaveSpec(spec))
	}
	labels := map[string]string{
		"the8020.owner":    updatedSpec.OwnerIDs[0],
		"the8020.owners":   strings.Join(updatedSpec.OwnerIDs, ","),
		"the8020.services": strings.Join(updatedSpec.ServiceIDs, ","),
	}
	if err := m.backend.UpdateLabels(ctx, updatedSpec.SandboxID, labels); err != nil {
		rollbackErr := errors.Join(m.store.SaveSpec(spec), m.store.SaveStatus(runtimeGroupID, status))
		return false, errors.Join(fmt.Errorf("update runtime-group owner labels: %w", err), rollbackErr)
	}
	return false, nil
}

func removeString(values []string, candidate string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != candidate {
			result = append(result, value)
		}
	}
	return result
}

func (m *Manager) List() ([]Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, err := m.store.List()
	if err != nil {
		return nil, err
	}
	result := make([]Inspection, 0, len(ids))
	for _, id := range ids {
		spec, status, loadErr := m.store.Load(id)
		if loadErr != nil {
			return nil, loadErr
		}
		result = append(result, Inspection{Spec: spec, Status: status})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Spec.SandboxID < result[j].Spec.SandboxID })
	return result, nil
}

func (m *Manager) Inspect(ctx context.Context, sandboxID string) (Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, status, err := m.find(sandboxID)
	if err != nil {
		return Inspection{}, err
	}
	workers, workerErr := m.supervisor.Workers(ctx, spec)
	metrics, metricsErr := m.sampleMetrics(ctx, spec, status.Metrics)
	if workerErr == nil || metricsErr == nil {
		if updated, updateErr := m.store.UpdateStatus(spec.RuntimeGroupID, func(value *model.SandboxStatus) error {
			if workerErr == nil {
				value.WorkerCount = len(workers)
			}
			if metricsErr == nil {
				value.Metrics = metrics
			}
			return nil
		}); updateErr == nil {
			status = updated
		}
	}
	return Inspection{Spec: spec, Status: status, Workers: workers}, nil
}

func (m *Manager) Metrics(sandboxID string) (model.ResourceMetrics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, status, err := m.find(sandboxID)
	if err != nil {
		return model.ResourceMetrics{}, err
	}
	metrics, err := m.sampleMetrics(context.Background(), spec, status.Metrics)
	if err != nil {
		return metrics, err
	}
	if _, err := m.store.UpdateStatus(spec.RuntimeGroupID, func(value *model.SandboxStatus) error {
		value.Metrics = metrics
		return nil
	}); err != nil {
		return metrics, err
	}
	return metrics, nil
}

// OpenConsole launches one new interactive process in a currently ready
// sandbox through the selected backend.
func (m *Manager) OpenConsole(ctx context.Context, sandboxID string, options backend.ConsoleOptions) (backend.Console, error) {
	m.mu.Lock()
	spec, status, err := m.find(sandboxID)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if status.ObservedState != model.StateReady {
		m.mu.Unlock()
		return nil, errors.New("sandbox is not ready")
	}
	consoleBackend, ok := m.backend.(backend.ConsoleBackend)
	if !ok {
		m.mu.Unlock()
		return nil, errors.New("selected sandbox backend does not support interactive consoles")
	}
	m.mu.Unlock()
	return consoleBackend.OpenConsole(ctx, spec.SandboxID, options)
}

// CheckHealth combines sandbox task state, supervisor heartbeat age, and
// available OOM evidence. Terminal records and logs move to history after the
// runtime side effects are cleaned.
func (m *Manager) CheckHealth(ctx context.Context, heartbeatTimeout time.Duration) (HealthReport, error) {
	if heartbeatTimeout <= 0 {
		return HealthReport{}, errors.New("positive supervisor heartbeat timeout is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	report := HealthReport{}
	observations, err := m.backend.List(ctx)
	if err != nil {
		return report, err
	}
	observed := make(map[string]backend.Observation, len(observations))
	for _, observation := range observations {
		observed[observation.RuntimeGroupID] = observation
	}
	ids, err := m.store.List()
	if err != nil {
		return report, err
	}
	for _, id := range ids {
		spec, status, loadErr := m.store.Load(id)
		if loadErr != nil {
			return report, loadErr
		}
		if status.ObservedState == model.StateFailed || status.ObservedState == model.StateStopped || status.ObservedState == model.StateStopping || status.ObservedState == model.StateDeleting {
			continue
		}
		report.Checked++
		observation, exists := observed[id]
		if !exists {
			failure, failed := m.failHealth(ctx, spec, "sandbox runtime or task is missing", false, false, nil)
			if failed {
				report.Failures = append(report.Failures, failure)
			}
			continue
		}
		if observation.TaskStatus != "running" {
			reason := fmt.Sprintf("sandbox task is not running: %s", observation.TaskStatus)
			failure, failed := m.failHealth(ctx, spec, reason, false, false, nil)
			if failed {
				report.Failures = append(report.Failures, failure)
			}
			continue
		}
		metrics, metricsErr := m.sampleMetrics(ctx, spec, status.Metrics)
		status, err = m.store.UpdateStatus(id, func(current *model.SandboxStatus) error {
			current.ContainerID, current.TaskPID = observation.ContainerID, observation.TaskPID
			if metricsErr == nil {
				current.Metrics = metrics
			}
			return nil
		})
		if err != nil {
			return report, err
		}
		if metricsErr == nil && (metrics.MemoryEvents["oom_kill"] > 0 || metrics.MemoryEvents["oom_group_kill"] > 0) {
			reason := fmt.Sprintf("cgroup OOM kill detected (oom_kill=%d, oom_group_kill=%d)", metrics.MemoryEvents["oom_kill"], metrics.MemoryEvents["oom_group_kill"])
			failure, failed := m.failHealth(ctx, spec, reason, true, true, nil)
			if failed {
				report.Failures = append(report.Failures, failure)
			}
			continue
		}
		if status.LastHeartbeat.IsZero() || m.now().Sub(status.LastHeartbeat) > heartbeatTimeout {
			reason := fmt.Sprintf("supervisor heartbeat exceeded %s", heartbeatTimeout)
			cutoff := m.now().Add(-heartbeatTimeout)
			failure, failed := m.failHealth(ctx, spec, reason, false, true, func(current model.SandboxStatus) bool {
				return current.LastHeartbeat.IsZero() || current.LastHeartbeat.Before(cutoff)
			})
			if failed {
				report.Failures = append(report.Failures, failure)
			}
			continue
		}
	}
	sort.Slice(report.Failures, func(i, j int) bool { return report.Failures[i].RuntimeGroupID < report.Failures[j].RuntimeGroupID })
	return report, nil
}

func (m *Manager) Stop(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(ctx, sandboxID, false)
}

func (m *Manager) Kill(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(ctx, sandboxID, true)
}

func (m *Manager) Delete(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteLocked(ctx, sandboxID)
}

func (m *Manager) deleteLocked(ctx context.Context, sandboxID string) error {
	spec, status, err := m.find(sandboxID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.ObservedState != model.StateStopped && status.ObservedState != model.StateFailed && status.ObservedState != model.StateDeleting {
		if err := m.stopSpecLocked(ctx, spec, status, true); err != nil {
			return err
		}
		_, status, err = m.store.Load(spec.RuntimeGroupID)
		if err != nil {
			return err
		}
	}
	if status.ObservedState != model.StateDeleting {
		if _, err := m.store.Transition(spec.RuntimeGroupID, model.StateDeleting, func(value *model.SandboxStatus) { value.DesiredState = model.StateDeleting }); err != nil {
			return err
		}
	}
	portErr := m.ports.CloseForSandbox(spec.SandboxID)
	if portErr != nil {
		return portErr
	}
	if err := m.backend.Delete(ctx, spec.SandboxID); err != nil {
		return err
	}
	if err := m.network.Release(ctx, spec.RuntimeGroupID); err != nil {
		return err
	}
	_, terminal, err := m.store.Load(spec.RuntimeGroupID)
	if err != nil {
		return err
	}
	if _, err := m.history.Archive(spec, terminal, "sandbox deleted", m.historyRetention); err != nil {
		return fmt.Errorf("archive deleted sandbox: %w", err)
	}
	return m.store.Delete(spec.RuntimeGroupID)
}

func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var report ReconcileReport
	observations, err := m.backend.List(ctx)
	if err != nil {
		return report, err
	}
	observed := make(map[string]backend.Observation, len(observations))
	for _, observation := range observations {
		observed[observation.RuntimeGroupID] = observation
	}
	ids, err := m.store.List()
	if err != nil {
		return report, err
	}
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
		spec, status, loadErr := m.store.Load(id)
		if loadErr != nil {
			report.Failed = append(report.Failed, id)
			continue
		}
		observation, exists := observed[id]
		if !exists {
			report.Missing = append(report.Missing, id)
			if failure, failed := m.failHealth(ctx, spec, "sandbox runtime or task is missing", false, false, nil); failed {
				report.Terminated = append(report.Terminated, failure)
			}
			continue
		}
		if observation.TaskStatus != "running" {
			report.Failed = append(report.Failed, id)
			if failure, failed := m.failHealth(ctx, spec, "sandbox task is not running", false, false, nil); failed {
				report.Terminated = append(report.Terminated, failure)
			}
			continue
		}
		if networkErr := m.network.Check(ctx, id); networkErr != nil {
			report.Failed = append(report.Failed, id)
			if failure, failed := m.failHealth(ctx, spec, "CNI check failed: "+networkErr.Error(), false, true, nil); failed {
				report.Terminated = append(report.Terminated, failure)
			}
			continue
		}
		supervisorStatus, supervisorErr := m.supervisor.Status(ctx, spec)
		if supervisorErr != nil {
			report.Failed = append(report.Failed, id)
			if failure, failed := m.failHealth(ctx, spec, "supervisor check failed: "+supervisorErr.Error(), false, true, nil); failed {
				report.Terminated = append(report.Terminated, failure)
			}
			continue
		}
		status.ContainerID, status.TaskPID = observation.ContainerID, observation.TaskPID
		status.SupervisorHealthy, status.SupervisorVersion, status.DenoVersion = true, supervisorStatus.SupervisorVersion, supervisorStatus.DenoVersion
		status.WorkerCount, status.LastHeartbeat = supervisorStatus.WorkerCount, m.now()
		target := model.StateReady
		if supervisorStatus.WorkerCount > 0 {
			target = model.StateActive
		}
		if err := m.restoreAvailable(id, status, target); err != nil {
			report.Failed = append(report.Failed, id)
			if failure, failed := m.failHealth(ctx, spec, "cannot reconcile lifecycle: "+err.Error(), false, true, nil); failed {
				report.Terminated = append(report.Terminated, failure)
			}
			continue
		}
		report.Restored++
	}
	for _, observation := range observations {
		if known[observation.RuntimeGroupID] {
			continue
		}
		if err := m.backend.Delete(ctx, observation.ContainerID); err != nil {
			return report, err
		}
		if err := m.ports.CloseForSandbox(observation.ContainerID); err != nil {
			return report, err
		}
		if observation.RuntimeGroupID != "" {
			_ = m.network.Release(ctx, observation.RuntimeGroupID)
		}
		report.OrphansDeleted = append(report.OrphansDeleted, observation.ContainerID)
	}
	sort.Strings(report.Missing)
	sort.Strings(report.Failed)
	sort.Strings(report.OrphansDeleted)
	return report, nil
}

// Startup applies the configured policy to runtimes left by an earlier kernel
// process. Reconcile verifies health for explicit preservation; destroy uses
// ownership metadata to remove inherited objects without health probes.
func (m *Manager) Startup(ctx context.Context, policy StartupPolicy) (ReconcileReport, error) {
	if policy != StartupReconcile && policy != StartupDestroy {
		return ReconcileReport{}, fmt.Errorf("unknown runtime startup policy %q", policy)
	}
	if policy == StartupReconcile {
		return m.Reconcile(ctx)
	}
	return m.destroyInherited(ctx)
}

// destroyInherited force-deletes every instance-owned backend object and its
// persisted record without probing task, network, or supervisor health. It is
// the fast crash-restart path; normal reconciliation remains available through
// the explicit reconcile startup policy.
func (m *Manager) destroyInherited(ctx context.Context) (ReconcileReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var report ReconcileReport
	owned, err := m.backend.ListOwned(ctx)
	if err != nil {
		return report, fmt.Errorf("list inherited sandboxes: %w", err)
	}
	ids, err := m.store.List()
	if err != nil {
		return report, fmt.Errorf("list persisted runtime groups: %w", err)
	}
	knownGroups := make(map[string]bool, len(ids))
	for _, id := range ids {
		knownGroups[id] = true
	}
	cleanedSandboxes := make(map[string]bool, len(owned))
	cleanedGroups := make(map[string]bool, len(owned))
	failedGroups := make(map[string]bool)
	var joined error
	cleanup := func(sandboxID, runtimeGroupID string) error {
		var cleanupErr error
		if err := m.backend.Delete(ctx, sandboxID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("force-delete sandbox %s: %w", sandboxID, err))
		}
		if err := m.ports.CloseForSandbox(sandboxID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release ports for sandbox %s: %w", sandboxID, err))
		}
		if runtimeGroupID != "" {
			if err := m.network.Release(ctx, runtimeGroupID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release network for runtime group %s: %w", runtimeGroupID, err))
			}
		}
		return cleanupErr
	}
	for _, observation := range owned {
		cleanupErr := cleanup(observation.ContainerID, observation.RuntimeGroupID)
		cleanedSandboxes[observation.ContainerID] = cleanupErr == nil
		if observation.RuntimeGroupID != "" {
			cleanedGroups[observation.RuntimeGroupID] = cleanupErr == nil
			if cleanupErr != nil {
				failedGroups[observation.RuntimeGroupID] = true
			}
		}
		if !knownGroups[observation.RuntimeGroupID] && cleanupErr == nil {
			report.OrphansDeleted = append(report.OrphansDeleted, observation.ContainerID)
		}
		joined = errors.Join(joined, cleanupErr)
	}
	for _, id := range ids {
		spec, status, loadErr := m.store.Load(id)
		if loadErr == nil && !cleanedSandboxes[spec.SandboxID] && !cleanedGroups[id] {
			cleanupErr := cleanup(spec.SandboxID, id)
			if cleanupErr != nil {
				failedGroups[id] = true
			}
			joined = errors.Join(joined, cleanupErr)
		}
		if failedGroups[id] {
			continue
		}
		if loadErr != nil {
			joined = errors.Join(joined, fmt.Errorf("load persisted runtime group %s for history: %w", id, loadErr))
			continue
		}
		if _, err := m.history.Archive(spec, status, "sandbox cleaned during kernel startup", m.historyRetention); err != nil {
			joined = errors.Join(joined, fmt.Errorf("archive inherited runtime group %s: %w", id, err))
			continue
		}
		report.Terminated = append(report.Terminated, HealthFailure{RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, Reason: "sandbox cleaned during kernel startup"})
		if err := m.store.Delete(id); err != nil {
			joined = errors.Join(joined, fmt.Errorf("delete persisted runtime group %s: %w", id, err))
		}
	}
	sort.Strings(report.OrphansDeleted)
	return report, joined
}

func (m *Manager) readMetrics(ctx context.Context, spec model.SandboxSpec) (model.ResourceMetrics, error) {
	if provider, ok := m.backend.(backend.MetricsProvider); ok {
		return provider.Metrics(ctx, spec.SandboxID)
	}
	directory := filepath.Join(m.cgroupRoot, "the8020", m.instanceUUID, spec.SandboxID)
	return resources.ReadMetrics(directory)
}

func (m *Manager) sampleMetrics(ctx context.Context, spec model.SandboxSpec, previous model.ResourceMetrics) (model.ResourceMetrics, error) {
	metrics, err := m.readMetrics(ctx, spec)
	if err != nil {
		return metrics, err
	}
	now := m.now()
	metrics.SampledAt = now
	if spec.ResourceLimits.MemoryMaximum > 0 {
		metrics.MemoryUtilization = float64(metrics.MemoryCurrent) / float64(spec.ResourceLimits.MemoryMaximum)
	}
	if !previous.SampledAt.IsZero() && now.After(previous.SampledAt) && metrics.CPUUsageMicros >= previous.CPUUsageMicros {
		quotaCores := float64(spec.ResourceLimits.CPUQuotaMicros) / float64(spec.ResourceLimits.CPUPeriodMicros)
		elapsedMicros := float64(now.Sub(previous.SampledAt).Microseconds())
		if quotaCores > 0 && elapsedMicros > 0 {
			metrics.CPUUtilization = float64(metrics.CPUUsageMicros-previous.CPUUsageMicros) / elapsedMicros / quotaCores
		}
	}
	return metrics, nil
}

func (m *Manager) restoreAvailable(runtimeGroupID string, status model.SandboxStatus, target model.SandboxState) error {
	current := status.ObservedState
	transition := func(next model.SandboxState) error {
		updated, err := m.store.Transition(runtimeGroupID, next, func(value *model.SandboxStatus) {
			value.ContainerID = status.ContainerID
			value.TaskPID = status.TaskPID
			value.SupervisorHealthy = status.SupervisorHealthy
			value.SupervisorVersion = status.SupervisorVersion
			value.DenoVersion = status.DenoVersion
			value.WorkerCount = status.WorkerCount
			value.LastHeartbeat = status.LastHeartbeat
			value.FailureReason = ""
		})
		if err == nil {
			current = updated.ObservedState
		}
		return err
	}
	if current == model.StateCreating {
		if err := transition(model.StateStarting); err != nil {
			return err
		}
	}
	if current == model.StateStarting {
		if err := transition(model.StateReady); err != nil {
			return err
		}
	}
	if current != target {
		if err := transition(target); err != nil {
			return err
		}
	} else {
		if err := transition(target); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context, policy ShutdownPolicy) error {
	if policy == ShutdownLeave {
		return nil
	}
	if policy != ShutdownDestroy {
		return fmt.Errorf("unknown runtime shutdown policy %q", policy)
	}
	items, err := m.List()
	if err != nil {
		return err
	}
	var joined error
	for _, item := range items {
		if err := m.Delete(ctx, item.Spec.SandboxID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (m *Manager) waitReady(ctx context.Context, spec model.SandboxSpec) (supervisor.Status, error) {
	readyContext, cancel := context.WithTimeout(ctx, m.startupTimeout)
	defer cancel()
	ticker := time.NewTicker(m.probeInterval)
	defer ticker.Stop()
	var lastError error
	for {
		status, err := m.supervisor.Status(readyContext, spec)
		if err == nil && !status.Draining {
			return status, nil
		}
		if err != nil {
			lastError = err
		}
		select {
		case <-readyContext.Done():
			if lastError != nil {
				return supervisor.Status{}, fmt.Errorf("supervisor did not become ready: %w", lastError)
			}
			return supervisor.Status{}, fmt.Errorf("supervisor did not become ready: %w", readyContext.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) stopLocked(ctx context.Context, sandboxID string, immediate bool) error {
	spec, status, err := m.find(sandboxID)
	if err != nil {
		return err
	}
	return m.stopSpecLocked(ctx, spec, status, immediate)
}

func (m *Manager) stopSpecLocked(ctx context.Context, spec model.SandboxSpec, status model.SandboxStatus, immediate bool) error {
	if status.ObservedState == model.StateStopped {
		return m.ports.CloseForSandbox(spec.SandboxID)
	}
	if !immediate && (status.ObservedState == model.StateReady || status.ObservedState == model.StateActive) {
		if _, err := m.store.Transition(spec.RuntimeGroupID, model.StateDraining, func(value *model.SandboxStatus) { value.DesiredState = model.StateStopped }); err != nil {
			return err
		}
		_ = m.supervisor.Drain(ctx, spec)
		status.ObservedState = model.StateDraining
	}
	if status.ObservedState != model.StateStopping {
		if _, err := m.store.Transition(spec.RuntimeGroupID, model.StateStopping, func(value *model.SandboxStatus) { value.DesiredState = model.StateStopped }); err != nil {
			return err
		}
	}
	portErr := m.ports.CloseForSandbox(spec.SandboxID)
	var err error
	if immediate {
		err = m.backend.Kill(ctx, spec.SandboxID)
	} else {
		err = m.backend.Stop(ctx, spec.SandboxID, m.stopGrace)
	}
	if err != nil {
		return errors.Join(portErr, err)
	}
	_, err = m.store.Transition(spec.RuntimeGroupID, model.StateStopped, func(value *model.SandboxStatus) { value.SupervisorHealthy = false; value.WorkerCount = 0 })
	return errors.Join(portErr, err)
}

func (m *Manager) find(sandboxID string) (model.SandboxSpec, model.SandboxStatus, error) {
	ids, err := m.store.List()
	if err != nil {
		return model.SandboxSpec{}, model.SandboxStatus{}, err
	}
	for _, id := range ids {
		spec, status, loadErr := m.store.Load(id)
		if loadErr != nil {
			return spec, status, loadErr
		}
		if spec.SandboxID == sandboxID || spec.RuntimeGroupID == sandboxID {
			return spec, status, nil
		}
	}
	return model.SandboxSpec{}, model.SandboxStatus{}, fmt.Errorf("sandbox %q: %w", sandboxID, os.ErrNotExist)
}

func (m *Manager) failCreate(spec model.SandboxSpec, cause error) error {
	if _, _, err := m.store.Load(spec.RuntimeGroupID); err == nil {
		_, _ = m.failHealth(context.Background(), spec, cause.Error(), false, false, nil)
	}
	return cause
}

func (m *Manager) failHealth(ctx context.Context, spec model.SandboxSpec, reason string, oom, kill bool, condition func(model.SandboxStatus) bool) (HealthFailure, bool) {
	terminal, transitioned, err := m.store.TransitionIf(spec.RuntimeGroupID, model.StateFailed, condition, func(value *model.SandboxStatus) {
		value.DesiredState = model.StateFailed
		value.SupervisorHealthy = false
		value.FailureReason = reason
	})
	if err != nil {
		reason += "; mark sandbox failed: " + err.Error()
		return HealthFailure{RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, Reason: reason, OOM: oom}, true
	}
	if !transitioned {
		return HealthFailure{}, false
	}
	var cleanupErr error
	reason, cleanupErr = m.cleanupFailed(ctx, spec, reason, kill)
	terminal, err = m.store.UpdateStatus(spec.RuntimeGroupID, func(value *model.SandboxStatus) error {
		value.FailureReason = reason
		return nil
	})
	if err != nil {
		reason += "; preserve terminal status: " + err.Error()
	} else if cleanupErr != nil {
		// Keep the failed record in the live catalog until cleanup can be retried
		// explicitly; history represents only completed runtime cleanup.
	} else if _, err := m.history.Archive(spec, terminal, reason, m.historyRetention); err != nil {
		reason += "; archive sandbox history: " + err.Error()
		_, _ = m.store.UpdateStatus(spec.RuntimeGroupID, func(value *model.SandboxStatus) error {
			value.FailureReason = reason
			return nil
		})
	} else if err := m.store.Delete(spec.RuntimeGroupID); err != nil {
		reason += "; remove live sandbox state: " + err.Error()
	}
	return HealthFailure{RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID, Reason: reason, OOM: oom}, true
}

// cleanupFailed releases runtime side effects before history publication.
func (m *Manager) cleanupFailed(ctx context.Context, spec model.SandboxSpec, reason string, kill bool) (string, error) {
	var joined error
	if kill {
		if err := m.backend.Kill(ctx, spec.SandboxID); err != nil {
			reason += "; terminate sandbox: " + err.Error()
			joined = errors.Join(joined, err)
		}
	}
	if err := m.backend.Delete(ctx, spec.SandboxID); err != nil {
		reason += "; delete sandbox container and snapshot: " + err.Error()
		joined = errors.Join(joined, err)
	}
	if err := m.network.Release(ctx, spec.RuntimeGroupID); err != nil {
		reason += "; release sandbox network: " + err.Error()
		joined = errors.Join(joined, err)
	}
	if err := m.ports.CloseForSandbox(spec.SandboxID); err != nil {
		reason += "; release sandbox ports: " + err.Error()
		joined = errors.Join(joined, err)
	}
	return reason, joined
}
