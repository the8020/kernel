// Package webservices owns filesystem web-service reconciliation and the
// canonical kernel HTTP service boundary.
package webservices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"the8020/kernel/auth"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/runtime/callback"
	"the8020/kernel/sandbox/model"
)

const internalHeaderPrefix = "X-80-20-Internal-"

type RuntimePools interface {
	Start(context.Context, string, string, executionservices.Options) (executionservices.Record, error)
	List() ([]executionservices.Record, error)
	Inspect(string) (executionservices.Record, error)
	Scale(context.Context, string, int) (executionservices.Record, error)
	EnsureCapacity(context.Context, string) (executionservices.Record, error)
	ReconcileCapacity(context.Context, string) (executionservices.Record, error)
	OpenAPI(context.Context, string) (map[string]any, error)
	Dispatch(context.Context, string, *http.Request) (*http.Response, error)
	ProxyWebSocket(context.Context, string, http.ResponseWriter, *http.Request) error
	Stop(context.Context, string) error
	RemoveStopped(string) error
}

type BoundaryRouter interface {
	RegisterServiceBoundary(http.Handler) error
}

type Authentication interface {
	CookieName() string
	ValidateCookie(string) (auth.AuthContext, error)
}

type RuntimeRequestRegistrar interface {
	BeginRuntimeRequest(auth.RuntimeRequest) (func(), error)
}

type NodeRouter interface {
	LocalNodeID() string
	LocalReplicaIndexes(int) []int
	Proxy(string, http.ResponseWriter, *http.Request) error
	ProxyAvailable(http.ResponseWriter, *http.Request) (bool, error)
}

type Config struct {
	Definitions              *workspacepackages.Store
	Pools                    RuntimePools
	Router                   BoundaryRouter
	ObservedRoot             string
	ReconcileInterval        time.Duration
	StartupTimeout           time.Duration
	ReplicaScaleDownCooldown time.Duration
	Logger                   *slog.Logger
	Authentication           Authentication
	RuntimeRequests          RuntimeRequestRegistrar
	NodeID                   string
	PersistentRouteStatePath string
	Nodes                    NodeRouter
}

type State string

const (
	StateDiscovered      State = "DISCOVERED"
	StateDisabled        State = "DISABLED"
	StateIdle            State = "IDLE"
	StatePendingCapacity State = "PENDING_CAPACITY"
	StateStarting        State = "STARTING"
	StateReady           State = "READY"
	StateDegraded        State = "DEGRADED"
	StateRestarting      State = "RESTARTING"
	StateDraining        State = "DRAINING"
	StateStopped         State = "STOPPED"
	StateFailed          State = "FAILED"
)

type InstanceStatus struct {
	Index            int      `json:"index"`
	PoolID           string   `json:"pool_id"`
	RuntimeGroupID   string   `json:"runtime_group_id"`
	SandboxID        string   `json:"sandbox_id"`
	WorkerIDs        []string `json:"worker_ids"`
	ActiveRequests   int      `json:"active_requests"`
	ActiveExecutions int      `json:"active_executions"`
}

type Metrics struct {
	RequestCount    uint64            `json:"request_count"`
	ActiveRequests  int               `json:"active_requests"`
	QueuedRequests  int               `json:"queued_requests"`
	ResponseStatus  map[string]uint64 `json:"response_status_counts"`
	RequestDuration time.Duration     `json:"last_request_duration"`
	WorkerRestarts  uint64            `json:"worker_restarts"`
	StartupFailures uint64            `json:"startup_failures"`
	TimeoutCount    uint64            `json:"timeout_count"`
	BytesStreamed   uint64            `json:"bytes_streamed"`
}

type Status struct {
	ServiceID         string                                   `json:"service_id"`
	PackageID         string                                   `json:"package_id"`
	CanonicalBasePath string                                   `json:"canonical_base_path"`
	Description       string                                   `json:"description,omitempty"`
	ExecutionMode     string                                   `json:"execution_mode,omitempty"`
	AccessMode        string                                   `json:"access_mode,omitempty"`
	Enabled           bool                                     `json:"enabled"`
	DesiredGeneration uint64                                   `json:"desired_generation"`
	LoadedGeneration  uint64                                   `json:"loaded_generation"`
	State             State                                    `json:"state"`
	InstanceCount     int                                      `json:"instance_count"`
	WorkerCount       int                                      `json:"worker_count"`
	Instances         []InstanceStatus                         `json:"instances"`
	Entrypoint        string                                   `json:"source_entrypoint,omitempty"`
	Effective         workspacepackages.EffectiveConfiguration `json:"effective_configuration"`
	ValidationError   string                                   `json:"validation_error,omitempty"`
	LastStartupError  string                                   `json:"last_startup_error,omitempty"`
	CapacityResource  string                                   `json:"capacity_resource,omitempty"`
	CapacityReason    string                                   `json:"capacity_reason,omitempty"`
	LastRestartTime   time.Time                                `json:"last_restart_time,omitempty"`
	FailedGeneration  uint64                                   `json:"failed_generation,omitempty"`
	Metrics           Metrics                                  `json:"metrics"`
}

type ValidationResult struct {
	ServiceID string         `json:"service_id"`
	Valid     bool           `json:"valid"`
	OpenAPI   map[string]any `json:"openapi,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type ScaleOptions struct {
	ReplicasMinimum          *int
	ReplicasMaximum          *int
	WorkersPerReplicaMinimum *int
	WorkersPerReplicaMaximum *int
	ConcurrencyPerWorker     *int
	TargetUtilization        *float64
	KeepAlive                *string
	SandboxGroup             *string
}

type RequestOptions struct {
	Headers http.Header
	Body    io.Reader
	Timeout time.Duration
}

type RequestResult struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       string      `json:"body"`
}

type runtimeInstance struct {
	status    InstanceStatus
	active    int
	serviceID string
}

type runtimeService struct {
	status             Status
	instances          []*runtimeInstance
	rejected           bool
	rejectedGeneration uint64
}

type persistentDispatch struct {
	token      string
	record     persistentRoute
	instance   *runtimeInstance
	initial    bool
	remoteNode string
}

type Manager struct {
	definitions     *workspacepackages.Store
	pools           RuntimePools
	observed        string
	interval        time.Duration
	startup         time.Duration
	logger          *slog.Logger
	authentication  Authentication
	runtimeRequests RuntimeRequestRegistrar
	nodes           NodeRouter

	mu                       sync.Mutex
	services                 map[string]*runtimeService
	persistentInstances      map[string]*runtimeInstance
	persistentRoutes         *persistentRouteRegistry
	reconcile                sync.Mutex
	replicaUnderTarget       map[string]time.Time
	replicaScaleDownCooldown time.Duration
	cancel                   context.CancelFunc
	wait                     sync.WaitGroup
}

func New(config Config) (*Manager, error) {
	if config.Definitions == nil || config.Pools == nil || config.Router == nil {
		return nil, errors.New("definition store, runtime pools, and service boundary router are required")
	}
	if config.ObservedRoot == "" {
		return nil, errors.New("node-local observed service root is required")
	}
	if err := os.MkdirAll(config.ObservedRoot, 0o700); err != nil {
		return nil, err
	}
	if config.ReconcileInterval <= 0 {
		config.ReconcileInterval = time.Second
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = 30 * time.Second
	}
	if config.ReplicaScaleDownCooldown <= 0 {
		config.ReplicaScaleDownCooldown = 30 * time.Second
	}
	manager := &Manager{definitions: config.Definitions, pools: config.Pools, observed: config.ObservedRoot, interval: config.ReconcileInterval, startup: config.StartupTimeout, replicaScaleDownCooldown: config.ReplicaScaleDownCooldown, logger: config.Logger, authentication: config.Authentication, runtimeRequests: config.RuntimeRequests, nodes: config.Nodes, services: map[string]*runtimeService{}, persistentInstances: map[string]*runtimeInstance{}, replicaUnderTarget: map[string]time.Time{}, persistentRoutes: newPersistentRouteRegistry(config.NodeID, config.PersistentRouteStatePath)}
	if err := config.Router.RegisterServiceBoundary(manager); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) StartReconciler(parent context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	m.wait.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wait.Done()
		lastFailure := ""
		if err := m.reconcileAll(ctx, false); err != nil && m.logger != nil && !errors.Is(err, context.Canceled) {
			m.logger.Error("initial filesystem service reconciliation failed", "error", err)
			lastFailure = err.Error()
		}
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := m.reconcileMaintained(ctx)
				if err == nil {
					lastFailure = ""
				} else if m.logger != nil && !errors.Is(err, context.Canceled) && err.Error() != lastFailure {
					m.logger.Error("active service maintenance failed", "error", err)
					lastFailure = err.Error()
				}
			}
		}
	}()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		m.wait.Wait()
	}
	return nil
}

func (m *Manager) ReconcileAll(ctx context.Context) error {
	return m.reconcileAll(ctx, true)
}

func (m *Manager) reconcileAll(ctx context.Context, provision bool) error {
	m.reconcile.Lock()
	defer m.reconcile.Unlock()
	ids, err := m.definitions.ListStateServiceIDs()
	if err != nil {
		return err
	}
	var joined error
	for _, serviceID := range ids {
		if ctx.Err() != nil {
			return errors.Join(joined, ctx.Err())
		}
		if _, err := m.reconcileOneLocked(ctx, serviceID, provision); err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", serviceID, err))
		}
	}
	return joined
}

func (m *Manager) reconcileMaintained(ctx context.Context) error {
	m.reconcile.Lock()
	defer m.reconcile.Unlock()
	m.mu.Lock()
	ids := make([]string, 0, len(m.services))
	for serviceID, runtime := range m.services {
		if len(runtime.instances) > 0 || runtime.status.State == StatePendingCapacity {
			ids = append(ids, serviceID)
		}
	}
	m.mu.Unlock()
	sort.Strings(ids)
	var joined error
	for _, serviceID := range ids {
		if ctx.Err() != nil {
			return errors.Join(joined, ctx.Err())
		}
		if _, err := m.reconcileOneLocked(ctx, serviceID, false); err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", serviceID, err))
		}
	}
	return joined
}

func (m *Manager) Start(ctx context.Context, serviceID string) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	state, err := m.definitions.MutateState(ctx, serviceID, func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = true
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	m.logDesiredState("start", serviceID, state)
	return m.reconcileOne(ctx, serviceID)
}

func (m *Manager) Stop(ctx context.Context, serviceID string) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	state, err := m.definitions.MutateState(ctx, serviceID, func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = false
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	m.logDesiredState("stop", serviceID, state)
	return m.reconcileOne(ctx, serviceID)
}

func (m *Manager) Restart(ctx context.Context, serviceID string) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	state, err := m.definitions.MutateState(ctx, serviceID, func(state *workspacepackages.DesiredServiceState) error {
		state.Enabled = true
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	m.logDesiredState("restart", serviceID, state)
	return m.reconcileOne(ctx, serviceID)
}

// Reload increments the service generation without changing whether the
// service is enabled. Package synchronization uses it to replace only active
// capacity while preserving environment-owned desired state.
func (m *Manager) Reload(ctx context.Context, serviceID string) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	state, err := m.definitions.MutateState(ctx, serviceID, nil)
	if err != nil {
		return Status{}, err
	}
	m.logDesiredState("reload", serviceID, state)
	return m.reconcileOne(ctx, serviceID)
}

// Retire removes runtime capacity for a service whose package no longer
// declares it. Shared desired state remains available if a later package
// version restores the service.
func (m *Manager) Retire(ctx context.Context, serviceID string) error {
	if _, err := workspacepackages.ParseServiceID(serviceID); err != nil {
		return err
	}
	m.reconcile.Lock()
	defer m.reconcile.Unlock()
	records, err := m.pools.List()
	if err != nil {
		return err
	}
	poolIDs := map[string]bool{}
	for _, record := range records {
		if record.LogicalServiceID == serviceID {
			poolIDs[serviceID+"\x00"+record.ServiceID] = true
		}
	}
	m.mu.Lock()
	delete(m.services, serviceID)
	for poolID, instance := range m.persistentInstances {
		if instance.serviceID == serviceID {
			poolIDs[serviceID+"\x00"+poolID] = true
			delete(m.persistentInstances, poolID)
		}
	}
	m.mu.Unlock()
	m.persistentRoutes.discardService(serviceID)
	var joined error
	for key := range poolIDs {
		_, poolID, _ := strings.Cut(key, "\x00")
		if stopErr := m.pools.Stop(ctx, poolID); stopErr != nil && !errors.Is(stopErr, os.ErrNotExist) {
			joined = errors.Join(joined, stopErr)
			continue
		}
		if removeErr := m.pools.RemoveStopped(poolID); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			joined = errors.Join(joined, removeErr)
		}
	}
	identity, _ := workspacepackages.ParseServiceID(serviceID)
	observed := filepath.Join(m.observed, identity.Namespace, identity.Repository, identity.Service)
	if removeErr := os.RemoveAll(observed); removeErr != nil {
		joined = errors.Join(joined, removeErr)
	}
	if m.logger != nil {
		m.logger.Info("removed package service retired", "package_id", identity.PackageID(), "service_id", serviceID, "error", joined)
	}
	return joined
}

func (m *Manager) Scale(ctx context.Context, serviceID string, options ScaleOptions) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	state, err := m.definitions.MutateState(ctx, serviceID, func(state *workspacepackages.DesiredServiceState) error {
		if options.ReplicasMinimum != nil {
			state.Scaling.ReplicasMinimum = copyInt(options.ReplicasMinimum)
		}
		if options.ReplicasMaximum != nil {
			state.Scaling.ReplicasMaximum = copyInt(options.ReplicasMaximum)
		}
		if options.WorkersPerReplicaMinimum != nil {
			state.Scaling.WorkersPerReplicaMinimum = copyInt(options.WorkersPerReplicaMinimum)
		}
		if options.WorkersPerReplicaMaximum != nil {
			state.Scaling.WorkersPerReplicaMaximum = copyInt(options.WorkersPerReplicaMaximum)
		}
		if options.ConcurrencyPerWorker != nil {
			state.Execution.ConcurrencyPerWorker = copyInt(options.ConcurrencyPerWorker)
		}
		if options.TargetUtilization != nil {
			value := *options.TargetUtilization
			state.Scaling.TargetUtilization = &value
		}
		if options.KeepAlive != nil {
			value := *options.KeepAlive
			state.Execution.KeepAlive = &value
		}
		if options.SandboxGroup != nil {
			value := *options.SandboxGroup
			state.Placement.SandboxGroup = &value
		}
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	m.logDesiredState("scale", serviceID, state)
	return m.reconcileOne(ctx, serviceID)
}

func (m *Manager) logDesiredState(action, serviceID string, state workspacepackages.DesiredServiceState) {
	if m.logger == nil {
		return
	}
	identity, _ := workspacepackages.ParseServiceID(serviceID)
	m.logger.Info("service desired state changed", "action", action, "package_id", identity.PackageID(), "service_id", serviceID, "enabled", state.Enabled, "desired_generation", state.Generation)
}

func copyInt(value *int) *int { copied := *value; return &copied }

func (m *Manager) reconcileOne(ctx context.Context, serviceID string) (Status, error) {
	m.reconcile.Lock()
	defer m.reconcile.Unlock()
	return m.reconcileOneLocked(ctx, serviceID, true)
}

func (m *Manager) reconcileOneLocked(ctx context.Context, serviceID string, provision bool) (Status, error) {
	definition, err := m.definitions.ReadService(serviceID)
	if err != nil {
		return m.retainFailedGeneration(serviceID, 0, err)
	}
	if !definition.StateExists {
		return m.statusFromDefinition(definition, StateDiscovered), nil
	}
	if !definition.State.Enabled {
		return m.stopRuntime(ctx, definition)
	}
	m.mu.Lock()
	existing := m.services[serviceID]
	previous := append([]*runtimeInstance(nil), instancesOf(existing)...)
	sameGeneration := existing != nil && existing.status.LoadedGeneration == definition.State.Generation && (existing.status.State == StateReady || existing.status.State == StateDegraded || existing.status.State == StateIdle)
	rejectedGeneration := existing != nil && existing.rejected && existing.rejectedGeneration == definition.State.Generation
	m.mu.Unlock()
	if !provision && rejectedGeneration {
		return cloneStatus(existing.status), nil
	}
	if !provision && len(previous) == 0 {
		if existing != nil && existing.status.State == StateFailed && existing.status.DesiredGeneration == definition.State.Generation {
			return cloneStatus(existing.status), nil
		}
		if existing == nil || existing.status.State != StatePendingCapacity {
			idle := m.statusFromDefinition(definition, StateIdle)
			if existing != nil {
				idle.Metrics = cloneMetrics(existing.status.Metrics)
			}
			m.mu.Lock()
			m.services[serviceID] = &runtimeService{status: idle}
			m.mu.Unlock()
			_ = m.writeObserved(idle)
			cleanupErr := m.cleanupStaleGenerationPools(ctx, definition, nil)
			if cleanupErr != nil {
				m.logGenerationCleanupFailure(serviceID, cleanupErr)
			}
			return cloneStatus(idle), cleanupErr
		}
	}
	if sameGeneration {
		status, healthy, refreshErr := m.refreshGeneration(ctx, definition, existing, previous)
		if refreshErr != nil {
			return m.retainFailedGeneration(serviceID, definition.State.Generation, refreshErr)
		}
		if healthy {
			m.mu.Lock()
			active := append([]*runtimeInstance(nil), instancesOf(existing)...)
			m.mu.Unlock()
			cleanupErr := m.cleanupStaleGenerationPools(ctx, definition, active)
			if cleanupErr != nil {
				m.logGenerationCleanupFailure(serviceID, cleanupErr)
			}
			return status, cleanupErr
		}
	}
	startingState := StateStarting
	if len(previous) > 0 {
		startingState = StateRestarting
	}
	starting := m.statusFromDefinition(definition, startingState)
	if existing != nil {
		starting.LoadedGeneration = existing.status.LoadedGeneration
		starting.Metrics = cloneMetrics(existing.status.Metrics)
	}
	m.mu.Lock()
	m.services[serviceID] = &runtimeService{status: starting, instances: previous}
	m.mu.Unlock()
	_ = m.writeObserved(starting)
	if m.logger != nil {
		m.logger.Info("service generation preparing", "package_id", definition.Identity.PackageID(), "service_id", serviceID, "desired_generation", definition.State.Generation, "loaded_generation", starting.LoadedGeneration, "state", starting.State)
	}

	startupContext, cancel := context.WithTimeout(ctx, m.startup)
	defer cancel()
	prepared, preparationErrors := m.prepareGeneration(startupContext, definition)
	preparationFailure := errors.Join(preparationErrors...)
	if len(preparationErrors) > 0 && errors.Is(preparationFailure, executionservices.ErrInvalidServiceDefinition) {
		previousPools := make(map[string]bool, len(previous))
		for _, instance := range previous {
			previousPools[instance.status.PoolID] = true
		}
		var cleanupErr error
		for _, instance := range prepared {
			if previousPools[instance.status.PoolID] {
				continue
			}
			if err := m.pools.Stop(context.Background(), instance.status.PoolID); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, err)
				continue
			}
			if err := m.pools.RemoveStopped(instance.status.PoolID); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		cleanupErr = errors.Join(cleanupErr, m.cleanupStaleGenerationPools(ctx, definition, previous))
		if cleanupErr != nil {
			m.logGenerationCleanupFailure(serviceID, cleanupErr)
		}
		return m.retainRejectedGeneration(serviceID, definition.State.Generation, errors.Join(preparationFailure, cleanupErr))
	}
	if len(preparationErrors) > 0 && len(previous) > 0 {
		previousPools := make(map[string]bool, len(previous))
		for _, instance := range previous {
			previousPools[instance.status.PoolID] = true
		}
		for _, instance := range prepared {
			if !previousPools[instance.status.PoolID] {
				_ = m.pools.Stop(context.Background(), instance.status.PoolID)
			}
		}
		return m.retainFailedGeneration(serviceID, definition.State.Generation, errors.Join(preparationErrors...))
	}
	localMinimum := len(m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMinimum))
	if len(prepared) < localMinimum {
		if len(preparationErrors) == 0 {
			preparationErrors = append(preparationErrors, fmt.Errorf("ready local replicas %d are below assigned minimum %d", len(prepared), localMinimum))
		}
	}

	state := StateReady
	if localMinimum == 0 {
		state = StateIdle
	}
	failure := ""
	if len(preparationErrors) > 0 && len(prepared) == 0 && localMinimum > 0 {
		state, failure = StatePendingCapacity, errors.Join(preparationErrors...).Error()
	} else if len(preparationErrors) > 0 {
		state, failure = StateDegraded, errors.Join(preparationErrors...).Error()
	}
	status := m.statusFromDefinition(definition, state)
	if len(prepared) > 0 || localMinimum == 0 {
		status.LoadedGeneration = definition.State.Generation
	}
	status.LastRestartTime = time.Now().UTC()
	status.LastStartupError = failure
	status.Instances = statusesOf(prepared)
	status.InstanceCount, status.WorkerCount = len(prepared), workerCount(prepared)
	if existing != nil {
		status.Metrics = cloneMetrics(existing.status.Metrics)
		if existing.status.LoadedGeneration != 0 && existing.status.LoadedGeneration != status.LoadedGeneration {
			status.Metrics.WorkerRestarts += uint64(workerCount(previous))
		}
	}
	m.mu.Lock()
	if definition.Service.Execution.Mode == workspacepackages.ExecutionModePersistent && existing != nil && existing.status.LoadedGeneration != 0 && existing.status.LoadedGeneration != status.LoadedGeneration {
		for _, instance := range previous {
			m.persistentInstances[instance.status.PoolID] = instance
		}
	}
	m.services[serviceID] = &runtimeService{status: status, instances: prepared}
	m.mu.Unlock()
	_ = m.writeObserved(status)
	cleanupErr := m.cleanupStaleGenerationPools(ctx, definition, prepared)
	if cleanupErr != nil {
		m.logGenerationCleanupFailure(serviceID, cleanupErr)
	}
	if m.logger != nil {
		m.logger.Info("service generation ready", "service_id", serviceID, "desired_generation", definition.State.Generation, "instance_count", len(prepared), "worker_count", status.WorkerCount, "state", status.State)
	}
	if len(preparationErrors) > 0 || cleanupErr != nil {
		return cloneStatus(status), errors.Join(errors.Join(preparationErrors...), cleanupErr)
	}
	return cloneStatus(status), nil
}

func (m *Manager) cleanupStaleGenerationPools(ctx context.Context, definition workspacepackages.Definition, active []*runtimeInstance) error {
	records, err := m.pools.List()
	if err != nil {
		return fmt.Errorf("list runtime pools for generation cleanup: %w", err)
	}
	activeIDs := make(map[string]bool, len(active))
	for _, instance := range active {
		activeIDs[instance.status.PoolID] = true
	}
	var joined error
	for _, record := range records {
		ownedRelease := strings.HasPrefix(record.ReleaseID, "service-generation-") || record.ReleaseID == "service-validation"
		if record.LogicalServiceID != definition.Identity.ServiceID() || activeIDs[record.ServiceID] || !ownedRelease {
			continue
		}
		if record.State == "STOPPED" && len(record.WorkerIDs) == 0 {
			if removeErr := m.pools.RemoveStopped(record.ServiceID); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				joined = errors.Join(joined, fmt.Errorf("remove retired pool %s generation %d: %w", record.ServiceID, record.Generation, removeErr))
				continue
			}
			m.mu.Lock()
			delete(m.persistentInstances, record.ServiceID)
			m.mu.Unlock()
			continue
		}
		if definition.Service.Execution.Mode == workspacepackages.ExecutionModePersistent && definition.State.Enabled {
			if m.persistentRoutes.hasPool(record.ServiceID) {
				m.mu.Lock()
				if _, exists := m.persistentInstances[record.ServiceID]; !exists {
					m.persistentInstances[record.ServiceID] = &runtimeInstance{serviceID: record.LogicalServiceID, status: InstanceStatus{Index: record.Instance, PoolID: record.ServiceID, RuntimeGroupID: record.RuntimeGroupID, SandboxID: record.SandboxID, WorkerIDs: append([]string{}, record.WorkerIDs...)}}
				}
				m.mu.Unlock()
				continue
			}
		}
		drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
		stopErr := m.pools.Stop(drainContext, record.ServiceID)
		cancel()
		if stopErr != nil {
			joined = errors.Join(joined, fmt.Errorf("retire pool %s generation %d: %w", record.ServiceID, record.Generation, stopErr))
		} else {
			if removeErr := m.pools.RemoveStopped(record.ServiceID); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				joined = errors.Join(joined, fmt.Errorf("remove retired pool %s generation %d: %w", record.ServiceID, record.Generation, removeErr))
				continue
			}
			m.mu.Lock()
			delete(m.persistentInstances, record.ServiceID)
			m.mu.Unlock()
		}
	}
	return joined
}

func (m *Manager) logGenerationCleanupFailure(serviceID string, err error) {
	if m.logger != nil {
		m.logger.Error("service generation cleanup failed", "service_id", serviceID, "error", err)
	}
}

func (m *Manager) refreshGeneration(ctx context.Context, definition workspacepackages.Definition, service *runtimeService, instances []*runtimeInstance) (Status, bool, error) {
	assignedMinimum := m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMinimum)
	localMinimum := len(assignedMinimum)
	refreshed := make([]InstanceStatus, 0, len(instances))
	capacity := make(map[string]executionservices.Record, len(instances))
	for _, instance := range instances {
		record, err := m.pools.Inspect(instance.status.PoolID)
		if err != nil || record.State != "READY" || record.Generation != definition.State.Generation {
			return Status{}, false, nil
		}
		desiredWorkers := len(record.WorkerIDs)
		if desiredWorkers < definition.Effective.Scaling.WorkersPerReplicaMinimum {
			desiredWorkers = definition.Effective.Scaling.WorkersPerReplicaMinimum
		}
		if desiredWorkers > definition.Effective.Scaling.WorkersPerReplicaMaximum {
			desiredWorkers = definition.Effective.Scaling.WorkersPerReplicaMaximum
		}
		if desiredWorkers == len(record.WorkerIDs) {
			record, err = m.pools.ReconcileCapacity(ctx, instance.status.PoolID)
		} else {
			record, err = m.pools.Scale(ctx, instance.status.PoolID, desiredWorkers)
			if err == nil {
				record, err = m.pools.ReconcileCapacity(ctx, instance.status.PoolID)
			}
		}
		if err != nil {
			return Status{}, false, fmt.Errorf("reconcile Workers for instance %d: %w", instance.status.Index, err)
		}
		status := instance.status
		status.RuntimeGroupID = record.RuntimeGroupID
		status.SandboxID = record.SandboxID
		status.WorkerIDs = append([]string{}, record.WorkerIDs...)
		refreshed = append(refreshed, status)
		capacity[record.ServiceID] = record
	}
	m.mu.Lock()
	current := m.services[definition.Identity.ServiceID()]
	if current != service {
		m.mu.Unlock()
		return Status{}, false, nil
	}
	for index := range instances {
		instances[index].status = refreshed[index]
	}
	present := make(map[int]bool, len(instances))
	for _, instance := range instances {
		present[instance.status.Index] = true
	}
	var preparationErrors []error
	for _, index := range assignedMinimum {
		if present[index] {
			continue
		}
		instance, err := m.prepareReplica(ctx, definition, index)
		if err != nil {
			preparationErrors = append(preparationErrors, fmt.Errorf("instance %d: %w", index, err))
			continue
		}
		instances = append(instances, instance)
		present[index] = true
		if record, err := m.pools.Inspect(instance.status.PoolID); err == nil {
			capacity[record.ServiceID] = record
		}
	}
	complete := containsReplicaIndexes(instances, assignedMinimum)
	if complete {
		allowed := make(map[int]bool)
		for _, index := range m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMaximum) {
			allowed[index] = true
		}
		retained := make([]*runtimeInstance, 0, len(instances))
		for _, instance := range instances {
			if allowed[instance.status.Index] {
				retained = append(retained, instance)
			}
		}
		instances = retained
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].status.Index < instances[j].status.Index })
	current.instances = instances
	m.mu.Unlock()
	if complete {
		var err error
		instances, err = m.scaleDownReplica(ctx, definition, service, instances, capacity)
		if err != nil {
			return Status{}, false, err
		}
	}
	m.mu.Lock()
	current = m.services[definition.Identity.ServiceID()]
	if current != service {
		m.mu.Unlock()
		return Status{}, false, nil
	}
	current.status.State = StateReady
	if !complete && len(instances) > 0 {
		current.status.State = StateDegraded
	} else if !complete {
		current.status.State = StatePendingCapacity
	} else if localMinimum == 0 && len(instances) == 0 {
		current.status.State = StateIdle
	}
	current.status.Enabled = true
	current.status.DesiredGeneration = definition.State.Generation
	current.status.CapacityResource = ""
	current.status.CapacityReason = ""
	if complete {
		current.status.FailedGeneration = 0
		current.status.LastStartupError = ""
	}
	current.instances = instances
	current.status.Instances = statusesOf(instances)
	current.status.InstanceCount = len(instances)
	current.status.WorkerCount = workerCount(instances)
	status := cloneStatus(current.status)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	if len(preparationErrors) > 0 {
		return status, true, errors.Join(preparationErrors...)
	}
	return status, true, nil
}

func containsReplicaIndexes(instances []*runtimeInstance, required []int) bool {
	present := make(map[int]bool, len(instances))
	for _, instance := range instances {
		present[instance.status.Index] = true
	}
	for _, index := range required {
		if !present[index] {
			return false
		}
	}
	return true
}

func (m *Manager) scaleDownReplica(ctx context.Context, definition workspacepackages.Definition, service *runtimeService, instances []*runtimeInstance, capacity map[string]executionservices.Record) ([]*runtimeInstance, error) {
	serviceID := definition.Identity.ServiceID()
	localMinimum := len(m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMinimum))
	if len(instances) <= localMinimum {
		m.mu.Lock()
		delete(m.replicaUnderTarget, serviceID)
		m.mu.Unlock()
		return instances, nil
	}
	var candidate *runtimeInstance
	for index := len(instances) - 1; index >= 0; index-- {
		instance := instances[index]
		record, exists := capacity[instance.status.PoolID]
		m.mu.Lock()
		active := instance.active
		m.mu.Unlock()
		if !exists || record.OccupiedSlots != 0 || len(record.WorkerIDs) > definition.Effective.Scaling.WorkersPerReplicaMinimum || active != 0 {
			continue
		}
		if definition.Service.Execution.Mode == workspacepackages.ExecutionModePersistent && m.persistentRoutes.hasPool(instance.status.PoolID) {
			continue
		}
		candidate = instance
		break
	}
	if candidate == nil {
		m.mu.Lock()
		delete(m.replicaUnderTarget, serviceID)
		m.mu.Unlock()
		return instances, nil
	}

	m.mu.Lock()
	since, waiting := m.replicaUnderTarget[serviceID]
	if !waiting {
		m.replicaUnderTarget[serviceID] = time.Now().UTC()
		m.mu.Unlock()
		return instances, nil
	}
	if time.Since(since) < m.replicaScaleDownCooldown || m.services[serviceID] != service || candidate.active != 0 {
		m.mu.Unlock()
		return instances, nil
	}
	remaining := make([]*runtimeInstance, 0, len(service.instances)-1)
	for _, instance := range service.instances {
		if instance != candidate {
			remaining = append(remaining, instance)
		}
	}
	if len(remaining) < localMinimum {
		m.mu.Unlock()
		return instances, nil
	}
	service.instances = remaining
	m.replicaUnderTarget[serviceID] = time.Now().UTC()
	m.mu.Unlock()

	drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
	err := m.pools.Stop(drainContext, candidate.status.PoolID)
	cancel()
	if err != nil {
		m.mu.Lock()
		if m.services[serviceID] == service {
			service.instances = append(service.instances, candidate)
			sort.Slice(service.instances, func(i, j int) bool { return service.instances[i].status.Index < service.instances[j].status.Index })
		}
		m.mu.Unlock()
		return instances, fmt.Errorf("remove idle service replica %d: %w", candidate.status.Index, err)
	}
	if m.logger != nil {
		m.logger.Info("idle service replica removed", "service_id", serviceID, "instance", candidate.status.Index, "pool_id", candidate.status.PoolID, "sandbox_id", candidate.status.SandboxID)
	}
	return remaining, nil
}

func (m *Manager) prepareGeneration(ctx context.Context, definition workspacepackages.Definition) ([]*runtimeInstance, []error) {
	indexes := m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMinimum)
	prepared := make([]*runtimeInstance, 0, len(indexes))
	var failures []error
	for _, index := range indexes {
		instance, err := m.prepareReplica(ctx, definition, index)
		if err != nil {
			failures = append(failures, fmt.Errorf("instance %d: %w", index, err))
			continue
		}
		prepared = append(prepared, instance)
	}
	return prepared, failures
}

func (m *Manager) localReplicaIndexes(limit int) []int {
	if m.nodes != nil {
		return m.nodes.LocalReplicaIndexes(limit)
	}
	result := make([]int, limit)
	for index := range result {
		result[index] = index
	}
	return result
}

func (m *Manager) prepareReplica(ctx context.Context, definition workspacepackages.Definition, index int) (*runtimeInstance, error) {
	poolID := generationPoolID(definition.Identity.ServiceID(), definition.State.Generation, index)
	record, err := m.pools.Start(ctx, poolID, definition.EntrypointURL, executionservices.Options{
		GroupKey:           definition.Effective.Placement.SandboxGroup,
		Namespace:          definition.Identity.Namespace,
		MinimumWorkers:     definition.Effective.Scaling.WorkersPerReplicaMinimum,
		MaximumWorkers:     definition.Effective.Scaling.WorkersPerReplicaMaximum,
		MaximumInFlight:    definition.Effective.Execution.ConcurrencyPerWorker,
		ReleaseID:          fmt.Sprintf("service-generation-%d", definition.State.Generation),
		LogicalServiceID:   definition.Identity.ServiceID(),
		Generation:         definition.State.Generation,
		CanonicalBasePath:  definition.Identity.CanonicalBasePath(),
		OpenAPI:            supervisor.OpenAPIMetadata{Title: definition.Service.OpenAPI.Title, Version: definition.Service.OpenAPI.Version, Description: coalesce(definition.Service.OpenAPI.Description, definition.Service.Description)},
		ValidateEntrypoint: true,
		Instance:           index,
		DependencyMode:     runtimeDependencyMode(definition.Effective.DependencyMode),
		ExecutionMode:      definition.Service.Execution.Mode,
		TargetUtilization:  definition.Effective.Scaling.TargetUtilization,
	})
	if err != nil {
		if existing, inspectErr := m.pools.Inspect(poolID); inspectErr == nil && existing.State == "READY" && existing.Generation == definition.State.Generation {
			record, err = existing, nil
		}
	}
	if err == nil && len(record.WorkerIDs) < definition.Effective.Scaling.WorkersPerReplicaMinimum {
		record, err = m.pools.Scale(ctx, poolID, definition.Effective.Scaling.WorkersPerReplicaMinimum)
	}
	if err == nil {
		_, err = m.pools.OpenAPI(ctx, poolID)
	}
	if err != nil {
		return nil, err
	}
	return &runtimeInstance{serviceID: definition.Identity.ServiceID(), status: InstanceStatus{Index: index, PoolID: poolID, RuntimeGroupID: record.RuntimeGroupID, SandboxID: record.SandboxID, WorkerIDs: append([]string{}, record.WorkerIDs...)}}, nil
}

func (m *Manager) stopRuntime(ctx context.Context, definition workspacepackages.Definition) (Status, error) {
	m.mu.Lock()
	existing := m.services[definition.Identity.ServiceID()]
	instances := append([]*runtimeInstance(nil), instancesOf(existing)...)
	alreadyStopped := existing != nil && existing.status.State == StateStopped && !existing.status.Enabled && existing.status.DesiredGeneration == definition.State.Generation && len(instances) == 0
	status := m.statusFromDefinition(definition, StateDisabled)
	if existing != nil {
		status.LoadedGeneration = existing.status.LoadedGeneration
		status.Metrics = cloneMetrics(existing.status.Metrics)
	}
	if len(instances) > 0 {
		status.State = StateDraining
	}
	status.Instances = statusesOf(instances)
	status.InstanceCount, status.WorkerCount = len(instances), workerCount(instances)
	m.services[definition.Identity.ServiceID()] = &runtimeService{status: status}
	m.mu.Unlock()
	_ = m.writeObserved(status)
	var joined error
	for _, instance := range instances {
		drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
		joined = errors.Join(joined, m.pools.Stop(drainContext, instance.status.PoolID))
		cancel()
	}
	joined = errors.Join(joined, m.cleanupStaleGenerationPools(ctx, definition, nil))
	status.State, status.LoadedGeneration = StateStopped, 0
	status.Instances, status.InstanceCount, status.WorkerCount = nil, 0, 0
	if joined != nil {
		status.State, status.LastStartupError = StateFailed, joined.Error()
	}
	m.mu.Lock()
	m.services[definition.Identity.ServiceID()] = &runtimeService{status: status}
	m.mu.Unlock()
	_ = m.writeObserved(status)
	if m.logger != nil && (!alreadyStopped || joined != nil) {
		m.logger.Info("service stopped", "package_id", definition.Identity.PackageID(), "service_id", definition.Identity.ServiceID(), "desired_generation", definition.State.Generation, "state", status.State)
	}
	return cloneStatus(status), joined
}

func (m *Manager) retainFailedGeneration(serviceID string, generation uint64, cause error) (Status, error) {
	m.mu.Lock()
	existing := m.services[serviceID]
	if existing == nil {
		existing = &runtimeService{status: Status{ServiceID: serviceID, State: StateFailed, Metrics: emptyMetrics()}}
		m.services[serviceID] = existing
	}
	failureState := StateFailed
	if len(existing.instances) > 0 {
		failureState = StateDegraded
	}
	duplicate := existing.status.FailedGeneration == generation && existing.status.LastStartupError == cause.Error() && existing.status.State == failureState
	existing.status.State = failureState
	existing.status.DesiredGeneration = generation
	existing.status.FailedGeneration = generation
	existing.status.LastStartupError = cause.Error()
	if !duplicate {
		existing.status.Metrics.StartupFailures++
	}
	status := cloneStatus(existing.status)
	m.mu.Unlock()
	if !duplicate {
		_ = m.writeObserved(status)
	}
	if m.logger != nil && !duplicate {
		m.logger.Error("service generation failed", "service_id", serviceID, "desired_generation", generation, "loaded_generation", status.LoadedGeneration, "error", cause)
	}
	return status, cause
}

func (m *Manager) retainRejectedGeneration(serviceID string, generation uint64, cause error) (Status, error) {
	status, err := m.retainFailedGeneration(serviceID, generation, cause)
	m.mu.Lock()
	if current := m.services[serviceID]; current != nil && current.status.DesiredGeneration == generation {
		current.rejected = true
		current.rejectedGeneration = generation
		status = cloneStatus(current.status)
	}
	m.mu.Unlock()
	return status, err
}

func (m *Manager) List() ([]Status, error) {
	definitions, err := m.definitions.ListServices()
	if err != nil {
		return nil, err
	}
	result := make([]Status, 0, len(definitions))
	for _, item := range definitions {
		if !item.Valid {
			result = append(result, Status{ServiceID: item.ID, PackageID: item.PackageID, CanonicalBasePath: item.CanonicalBasePath, Description: item.Description, State: StateFailed, ValidationError: strings.Join(item.ValidationErrors, "; "), Metrics: emptyMetrics()})
			continue
		}
		status, inspectErr := m.Inspect(item.ID)
		if inspectErr != nil {
			return nil, inspectErr
		}
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ServiceID < result[j].ServiceID })
	return result, nil
}

func (m *Manager) Inspect(serviceID string) (Status, error) {
	definition, err := m.definitions.ReadService(serviceID)
	if err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	existing := m.services[serviceID]
	if existing != nil {
		status := cloneStatus(existing.status)
		status.Enabled, status.DesiredGeneration = definition.State.Enabled, definition.State.Generation
		status.Description, status.Entrypoint, status.Effective = definition.Service.Description, definition.EntrypointPath, definition.Effective
		status.Instances = statusesOf(existing.instances)
		status.InstanceCount, status.WorkerCount = len(existing.instances), workerCount(existing.instances)
		m.mu.Unlock()
		return status, nil
	}
	m.mu.Unlock()
	state := StateDiscovered
	if definition.StateExists {
		state = StateDisabled
	}
	return m.statusFromDefinition(definition, state), nil
}

func (m *Manager) Validate(ctx context.Context, serviceID string) ValidationResult {
	definition, err := m.definitions.ReadService(serviceID)
	if err != nil {
		return ValidationResult{ServiceID: serviceID, Error: err.Error()}
	}
	validationID := validationPoolID(serviceID)
	record, err := m.pools.Start(ctx, validationID, definition.EntrypointURL, executionservices.Options{
		GroupKey: "validation-" + validationID, Namespace: definition.Identity.Namespace,
		MinimumWorkers: 1, MaximumWorkers: 1, MaximumInFlight: 1,
		ReleaseID: "service-validation", LogicalServiceID: serviceID,
		Generation: definition.State.Generation, CanonicalBasePath: definition.Identity.CanonicalBasePath(),
		OpenAPI:            supervisor.OpenAPIMetadata{Title: definition.Service.OpenAPI.Title, Version: definition.Service.OpenAPI.Version, Description: coalesce(definition.Service.OpenAPI.Description, definition.Service.Description)},
		ValidateEntrypoint: true,
		DependencyMode:     runtimeDependencyMode(definition.Effective.DependencyMode),
		ExecutionMode:      definition.Service.Execution.Mode,
	})
	if err != nil {
		return ValidationResult{ServiceID: serviceID, Error: err.Error()}
	}
	defer m.retireValidationPool(record.ServiceID)
	document, err := m.pools.OpenAPI(ctx, record.ServiceID)
	if err != nil {
		if m.logger != nil {
			m.logger.Error("service OpenAPI generation failed", "service_id", serviceID, "error", err)
		}
		return ValidationResult{ServiceID: serviceID, Error: err.Error()}
	}
	return ValidationResult{ServiceID: serviceID, Valid: true, OpenAPI: document}
}

func (m *Manager) retireValidationPool(poolID string) {
	if err := m.pools.Stop(context.Background(), poolID); err != nil && !errors.Is(err, os.ErrNotExist) {
		if m.logger != nil {
			m.logger.Error("stop service validation pool", "service_pool_id", poolID, "error", err)
		}
		return
	}
	if err := m.pools.RemoveStopped(poolID); err != nil && !errors.Is(err, os.ErrNotExist) && m.logger != nil {
		m.logger.Error("remove service validation pool", "service_pool_id", poolID, "error", err)
	}
}

func runtimeDependencyMode(value string) model.DependencyMode {
	if value == "online" {
		return model.DependencyOnline
	}
	return model.DependencyCachedOnly
}

func (m *Manager) OpenAPI(ctx context.Context, serviceID string) (map[string]any, error) {
	m.mu.Lock()
	existing := m.services[serviceID]
	if existing != nil && len(existing.instances) > 0 {
		poolID := existing.instances[0].status.PoolID
		m.mu.Unlock()
		return m.pools.OpenAPI(ctx, poolID)
	}
	m.mu.Unlock()
	result := m.Validate(ctx, serviceID)
	if !result.Valid {
		return nil, errors.New(result.Error)
	}
	return result.OpenAPI, nil
}

func (m *Manager) Request(ctx context.Context, serviceID, method, relativePath string, options RequestOptions) (RequestResult, error) {
	identity, err := workspacepackages.ParseServiceID(serviceID)
	if err != nil {
		return RequestResult{}, err
	}
	if !strings.HasPrefix(relativePath, "/") {
		relativePath = "/" + relativePath
	}
	target := "http://the8020" + identity.CanonicalBasePath() + relativePath
	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), target, options.Body)
	if err != nil {
		return RequestResult{}, err
	}
	if options.Headers != nil {
		request.Header = options.Headers.Clone()
	}
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
		request = request.WithContext(ctx)
	}
	recorder := httptest.NewRecorder()
	m.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return RequestResult{}, err
	}
	return RequestResult{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Body: string(body)}, nil
}

func (m *Manager) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, relativePath, err := parseCanonicalRequest(request.URL)
	if err != nil {
		if errors.Is(err, errNotServicePath) {
			http.NotFound(writer, request)
		} else {
			http.Error(writer, "invalid service path", http.StatusBadRequest)
		}
		return
	}
	serviceID := identity.ServiceID()
	definition, definitionErr := m.definitions.ReadService(serviceID)
	m.mu.Lock()
	runtime := m.services[serviceID]
	hasCapacity := runtime != nil && len(runtime.instances) > 0
	rejectedGeneration := runtime != nil && runtime.rejected && runtime.rejectedGeneration == definition.State.Generation
	m.mu.Unlock()
	if definitionErr != nil {
		if !hasCapacity && errors.Is(definitionErr, os.ErrNotExist) {
			http.NotFound(writer, request)
		} else {
			// Access policy is read from the exact manifest for every request.
			// If it cannot be read, retained capacity must fail closed rather
			// than accidentally bypass a previously authenticated boundary.
			if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
				http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			}
		}
		return
	}
	if !definition.StateExists || !definition.State.Enabled {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	authContext := auth.AuthContext{}
	authContext, authenticated := m.authenticate(request)
	if definition.Service.Access.Mode == workspacepackages.AccessModeAuthenticated {
		if !authenticated {
			m.respondUnauthenticated(writer, definition.Service.Access.Unauthenticated)
			return
		}
	} else if !authenticated {
		authContext = auth.AuthContext{}
	}
	if !hasCapacity {
		if rejectedGeneration {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		_, reconcileErr := m.reconcileOne(request.Context(), serviceID)
		m.mu.Lock()
		runtime = m.services[serviceID]
		hasCapacity = runtime != nil && len(runtime.instances) > 0
		m.mu.Unlock()
		if reconcileErr != nil && !hasCapacity {
			if m.logger != nil {
				m.logger.Error("service cold start failed", "service_id", serviceID, "error", reconcileErr)
			}
			if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
				http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if reconcileErr != nil && m.logger != nil {
			m.logger.Warn("service cold start continuing with degraded capacity", "service_id", serviceID, "error", reconcileErr, "instance_count", len(runtime.instances))
		}
		if !hasCapacity {
			if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
				http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
	}
	if definition.Service.Execution.Mode == workspacepackages.ExecutionModePersistent {
		route, routeErr := m.beginPersistentDispatch(request.Context(), runtime, definition, request, authContext.UserID, isWebSocketUpgrade(request))
		if routeErr != nil {
			if errors.Is(routeErr, executionservices.ErrReplicaCapacity) {
				if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
					m.respondCapacityUnavailable(writer, routeErr)
				}
			} else {
				http.Error(writer, "persistent execution lost", http.StatusConflict)
			}
			return
		}
		if route.remoteNode != "" {
			if m.nodes == nil {
				http.Error(writer, "owning node unavailable", http.StatusServiceUnavailable)
				return
			}
			if err := m.nodes.Proxy(route.remoteNode, writer, request); err != nil {
				http.Error(writer, "owning node unavailable", http.StatusBadGateway)
			}
			return
		}
		if isWebSocketUpgrade(request) {
			m.dispatchPersistentWebSocket(writer, request, identity, relativePath, runtime, route, authContext)
		} else {
			m.dispatch(writer, request, identity, relativePath, runtime, route.instance, definition.Effective.Timeouts.Request, authContext, route)
		}
		return
	}
	instance, capacityErr := m.selectCapacityInstance(request.Context(), runtime, definition)
	if capacityErr != nil {
		if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
			m.respondCapacityUnavailable(writer, capacityErr)
		}
		return
	}
	if isWebSocketUpgrade(request) {
		m.dispatchRequestWebSocket(writer, request, identity, relativePath, runtime, instance, authContext)
		return
	}
	m.dispatch(writer, request, identity, relativePath, runtime, instance, definition.Effective.Timeouts.Request, authContext, nil)
}

func (m *Manager) dispatch(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, relativePath string, runtime *runtimeService, instance *runtimeInstance, timeout time.Duration, authContext auth.AuthContext, persistent *persistentDispatch) {
	if instance == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	requestID, err := model.NewID("request")
	if err != nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	started := time.Now()
	stopRouteKeepalive := m.keepPersistentRouteAlive(persistent)
	defer stopRouteKeepalive()
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	forwarded := request.Clone(ctx)
	forwarded.URL = cloneURL(request.URL)
	// Incoming server requests normally have only a path. Deno's Request
	// constructor requires the supervisor-provided service URL to be absolute.
	forwarded.URL.Scheme, forwarded.URL.Host = "http", "service"
	forwarded.URL.Path, forwarded.URL.RawPath = relativePath, ""
	query := forwarded.URL.Query()
	query.Del("route")
	forwarded.URL.RawQuery = query.Encode()
	forwarded.RequestURI = ""
	forwarded.Header = request.Header.Clone()
	if forwarded.Header == nil {
		forwarded.Header = make(http.Header)
	}
	for name := range forwarded.Header {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), internalHeaderPrefix) {
			forwarded.Header.Del(name)
		}
	}
	forwarded.Header.Del(RouteHeader)
	if m.authentication != nil {
		removeCookie(forwarded.Header, request.Cookies(), m.authentication.CookieName())
	}
	forwarded.Header.Set(internalHeaderPrefix+"Request-ID", requestID)
	forwarded.Header.Set(internalHeaderPrefix+"Service-ID", identity.ServiceID())
	m.mu.Lock()
	loadedGeneration := runtime.status.LoadedGeneration
	m.mu.Unlock()
	forwarded.Header.Set(internalHeaderPrefix+"Service-Generation", strconv.FormatUint(loadedGeneration, 10))
	forwarded.Header.Set(internalHeaderPrefix+"Canonical-Base-Path", identity.CanonicalBasePath())
	forwarded.Header.Set(internalHeaderPrefix+"Original-URL", originalURL(request))
	forwarded.Header.Set(internalHeaderPrefix+"Original-Path", request.URL.RequestURI())
	forwarded.Header.Set(internalHeaderPrefix+"Original-Host", request.Host)
	forwarded.Header.Set(internalHeaderPrefix+"Original-Scheme", requestScheme(request))
	if persistent != nil {
		forwarded.Header.Set(internalHeaderPrefix+"Persistent-Execution-ID", persistent.record.ExecutionID)
		forwarded.Header.Set(internalHeaderPrefix+"Persistent-Keep-Alive-MS", strconv.FormatInt(persistent.record.KeepAlive.Milliseconds(), 10))
		if persistent.record.WorkerID != "" {
			forwarded.Header.Set(internalHeaderPrefix+"Target-Worker-ID", persistent.record.WorkerID)
		}
		if !persistent.initial {
			forwarded.Header.Set(internalHeaderPrefix+"Persistent-Existing", "true")
		}
	}
	forwarded.Header.Set(internalHeaderPrefix+"Auth-Authenticated", strconv.FormatBool(authContext.Authenticated))
	if authContext.Authenticated {
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Realm", authContext.Realm)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-User-ID", authContext.UserID)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Username", authContext.Username)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Version", strconv.FormatUint(authContext.AuthVersion, 10))
	}
	if m.runtimeRequests != nil {
		release, registrationErr := m.runtimeRequests.BeginRuntimeRequest(auth.RuntimeRequest{
			RequestID: requestID, ServiceID: identity.ServiceID(), RuntimeGroupID: instance.status.RuntimeGroupID,
			SandboxID: instance.status.SandboxID, Auth: authContext, SecureTransport: requestScheme(request) == "https",
		})
		if registrationErr != nil {
			if persistent != nil && persistent.initial {
				m.persistentRoutes.discard(persistent.token)
			}
			m.finishRequest(runtime, instance, 0, 0, time.Since(started), false)
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		defer release()
	}
	response, dispatchErr := m.pools.Dispatch(ctx, instance.status.PoolID, forwarded)
	if dispatchErr != nil {
		if persistent != nil && persistent.initial {
			m.persistentRoutes.discard(persistent.token)
		}
		m.finishRequest(runtime, instance, 0, 0, time.Since(started), errors.Is(ctx.Err(), context.DeadlineExceeded))
		if m.logger != nil {
			m.logger.Error("service request dispatch failed", "service_id", identity.ServiceID(), "request_id", requestID, "error", dispatchErr)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			http.Error(writer, "service request timed out", http.StatusGatewayTimeout)
		} else {
			http.Error(writer, "service proxy failed", http.StatusBadGateway)
		}
		return
	}
	defer response.Body.Close()
	selectedWorkerID := response.Header.Get(internalHeaderPrefix + "Selected-Worker-ID")
	persistentSuccess := persistent != nil && response.StatusCode >= 200 && response.StatusCode < 400
	if persistentSuccess {
		m.persistentRoutes.succeed(persistent.token, selectedWorkerID)
		writer.Header().Set(RouteHeader, persistent.token)
	} else if persistent != nil && persistent.initial {
		m.persistentRoutes.discard(persistent.token)
	}
	for name, values := range response.Header {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), internalHeaderPrefix) {
			continue
		}
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	written, copyErr := io.Copy(writer, response.Body)
	duration := time.Since(started)
	m.finishRequest(runtime, instance, response.StatusCode, uint64(max64(written, 0)), duration, errors.Is(ctx.Err(), context.DeadlineExceeded))
	if m.logger != nil {
		m.logger.Info("service request completed", "package_id", identity.PackageID(), "service_id", identity.ServiceID(), "request_id", requestID, "runtime_group_id", instance.status.RuntimeGroupID, "sandbox_id", instance.status.SandboxID, "duration", duration, "status_code", response.StatusCode, "bytes_streamed", max64(written, 0))
	}
	if copyErr != nil && m.logger != nil {
		m.logger.Error("service response stream failed", "service_id", runtime.status.ServiceID, "request_id", requestID, "error", copyErr)
	}
}

func (m *Manager) dispatchPersistentWebSocket(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, relativePath string, runtime *runtimeService, persistent *persistentDispatch, authContext auth.AuthContext) {
	writer.Header().Set(RouteHeader, persistent.token)
	started := time.Now()
	stopRouteKeepalive := m.keepPersistentRouteAlive(persistent)
	defer stopRouteKeepalive()
	succeeded := m.dispatchWebSocket(writer, request, identity, runtime, persistent.instance, relativePath, authContext, persistent)
	m.persistentRoutes.disconnect(persistent.token, succeeded)
	if !succeeded && persistent.initial {
		m.persistentRoutes.discard(persistent.token)
	}
	m.finishRequest(runtime, persistent.instance, http.StatusSwitchingProtocols, 0, time.Since(started), false)
}

func (m *Manager) keepPersistentRouteAlive(persistent *persistentDispatch) func() {
	if persistent == nil || persistent.record.KeepAlive <= 0 {
		return func() {}
	}
	interval := max(persistent.record.KeepAlive/2, time.Millisecond)
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.persistentRoutes.succeed(persistent.token, "")
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func (m *Manager) dispatchRequestWebSocket(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, relativePath string, runtime *runtimeService, instance *runtimeInstance, authContext auth.AuthContext) {
	started := time.Now()
	defer func() {
		m.finishRequest(runtime, instance, http.StatusSwitchingProtocols, 0, time.Since(started), false)
	}()
	m.dispatchWebSocket(writer, request, identity, runtime, instance, relativePath, authContext, nil)
}

func (m *Manager) dispatchWebSocket(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, runtime *runtimeService, instance *runtimeInstance, relativePath string, authContext auth.AuthContext, persistent *persistentDispatch) bool {
	requestID, err := model.NewID("request")
	if err != nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	forwarded := request.Clone(request.Context())
	forwarded.URL = cloneURL(request.URL)
	forwarded.URL.Scheme, forwarded.URL.Host = "http", "service"
	forwarded.URL.Path, forwarded.URL.RawPath = relativePath, ""
	query := forwarded.URL.Query()
	query.Del("route")
	forwarded.URL.RawQuery = query.Encode()
	forwarded.RequestURI = ""
	forwarded.Header = request.Header.Clone()
	if forwarded.Header == nil {
		forwarded.Header = make(http.Header)
	}
	for name := range forwarded.Header {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), internalHeaderPrefix) {
			forwarded.Header.Del(name)
		}
	}
	forwarded.Header.Del(RouteHeader)
	if m.authentication != nil {
		removeCookie(forwarded.Header, request.Cookies(), m.authentication.CookieName())
	}
	forwarded.Header.Set(internalHeaderPrefix+"Request-ID", requestID)
	forwarded.Header.Set(internalHeaderPrefix+"Service-ID", identity.ServiceID())
	m.mu.Lock()
	loadedGeneration := runtime.status.LoadedGeneration
	m.mu.Unlock()
	forwarded.Header.Set(internalHeaderPrefix+"Service-Generation", strconv.FormatUint(loadedGeneration, 10))
	forwarded.Header.Set(internalHeaderPrefix+"Canonical-Base-Path", identity.CanonicalBasePath())
	forwarded.Header.Set(internalHeaderPrefix+"Original-URL", originalURL(request))
	forwarded.Header.Set(internalHeaderPrefix+"Original-Path", request.URL.RequestURI())
	forwarded.Header.Set(internalHeaderPrefix+"Original-Host", request.Host)
	forwarded.Header.Set(internalHeaderPrefix+"Original-Scheme", requestScheme(request))
	if persistent != nil {
		forwarded.Header.Set(internalHeaderPrefix+"Persistent-Execution-ID", persistent.record.ExecutionID)
		forwarded.Header.Set(internalHeaderPrefix+"Persistent-Keep-Alive-MS", strconv.FormatInt(persistent.record.KeepAlive.Milliseconds(), 10))
		if persistent.record.WorkerID != "" {
			forwarded.Header.Set(internalHeaderPrefix+"Target-Worker-ID", persistent.record.WorkerID)
		}
		if !persistent.initial {
			forwarded.Header.Set(internalHeaderPrefix+"Persistent-Existing", "true")
		}
	}
	forwarded.Header.Set(internalHeaderPrefix+"Auth-Authenticated", strconv.FormatBool(authContext.Authenticated))
	if authContext.Authenticated {
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Realm", authContext.Realm)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-User-ID", authContext.UserID)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Username", authContext.Username)
		forwarded.Header.Set(internalHeaderPrefix+"Auth-Version", strconv.FormatUint(authContext.AuthVersion, 10))
	}
	if m.runtimeRequests != nil {
		release, registrationErr := m.runtimeRequests.BeginRuntimeRequest(auth.RuntimeRequest{
			RequestID: requestID, ServiceID: identity.ServiceID(), RuntimeGroupID: instance.status.RuntimeGroupID,
			SandboxID: instance.status.SandboxID, Auth: authContext, SecureTransport: requestScheme(request) == "https",
		})
		if registrationErr != nil {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return false
		}
		defer release()
	}
	if err := m.pools.ProxyWebSocket(request.Context(), instance.status.PoolID, writer, forwarded); err != nil {
		if m.logger != nil {
			m.logger.Error("service WebSocket proxy failed", "service_id", identity.ServiceID(), "request_id", requestID, "pool_id", instance.status.PoolID, "error", err)
		}
		http.Error(writer, "service proxy failed", http.StatusBadGateway)
		return false
	}
	return true
}

func (m *Manager) authenticate(request *http.Request) (auth.AuthContext, bool) {
	if m.authentication == nil {
		return auth.AuthContext{}, false
	}
	cookie, err := request.Cookie(m.authentication.CookieName())
	if err != nil || cookie.Value == "" {
		return auth.AuthContext{}, false
	}
	context, err := m.authentication.ValidateCookie(cookie.Value)
	if err != nil {
		if m.logger != nil && !errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrSessionExpired) {
			m.logger.Warn("authentication validation failed", "error", err)
		}
		return auth.AuthContext{}, false
	}
	return context, context.Authenticated
}

func (m *Manager) respondUnauthenticated(writer http.ResponseWriter, policy workspacepackages.UnauthenticatedManifest) {
	writer.Header().Set("Cache-Control", "no-store")
	if policy.Action == workspacepackages.UnauthenticatedRedirect {
		writer.Header().Set("Location", policy.RedirectURL)
		writer.WriteHeader(policy.Status)
		return
	}
	http.Error(writer, policy.Message, policy.Status)
}

func removeCookie(header http.Header, cookies []*http.Cookie, name string) {
	if name == "" {
		return
	}
	header.Del("Cookie")
	values := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name != name {
			values = append(values, cookie.String())
		}
	}
	if len(values) > 0 {
		header.Set("Cookie", strings.Join(values, "; "))
	}
}

func (m *Manager) selectInstance(runtime *runtimeService) *runtimeInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || len(runtime.instances) == 0 {
		return nil
	}
	instances := append([]*runtimeInstance(nil), runtime.instances...)
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].active < instances[j].active || (instances[i].active == instances[j].active && instances[i].status.Index < instances[j].status.Index)
	})
	selected := instances[0]
	selected.active++
	selected.status.ActiveRequests = selected.active
	runtime.status.Metrics.ActiveRequests++
	return selected
}

func (m *Manager) selectCapacityInstance(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeInstance, error) {
	m.mu.Lock()
	instances := append([]*runtimeInstance(nil), instancesOf(runtime)...)
	m.mu.Unlock()
	sort.SliceStable(instances, func(i, j int) bool {
		return instances[i].active < instances[j].active || (instances[i].active == instances[j].active && instances[i].status.Index < instances[j].status.Index)
	})
	var fallback *runtimeInstance
	var lastErr error
	for _, instance := range instances {
		record, err := m.pools.EnsureCapacity(ctx, instance.status.PoolID)
		if err == nil {
			if activated := m.activateInstance(runtime, instance, record.WorkerIDs); activated != nil {
				return activated, nil
			}
			return nil, errors.New("service generation changed while selecting capacity")
		}
		lastErr = err
		var capacity *executionservices.ReplicaCapacityError
		if errors.As(err, &capacity) {
			if capacity.Occupied < capacity.Slots && fallback == nil {
				fallback = instance
			}
			continue
		}
	}

	instance, err := m.addCapacityReplica(ctx, runtime, definition)
	if err == nil {
		if activated := m.activateInstance(runtime, instance, instance.status.WorkerIDs); activated != nil {
			return activated, nil
		}
		return nil, errors.New("service generation changed while activating capacity")
	}
	lastErr = err
	if fallback != nil {
		if activated := m.activateInstance(runtime, fallback, fallback.status.WorkerIDs); activated != nil {
			return activated, nil
		}
		return nil, errors.New("service generation changed while selecting fallback capacity")
	}
	if lastErr == nil {
		lastErr = errors.New("all configured execution slots are occupied")
	}
	return nil, fmt.Errorf("%w: %v", executionservices.ErrReplicaCapacity, lastErr)
}

func (m *Manager) addCapacityReplica(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeInstance, error) {
	m.reconcile.Lock()
	defer m.reconcile.Unlock()
	m.mu.Lock()
	current := m.services[definition.Identity.ServiceID()]
	if current != runtime {
		m.mu.Unlock()
		return nil, errors.New("service generation changed while adding capacity")
	}
	used := make(map[int]bool, len(current.instances))
	for _, instance := range current.instances {
		used[instance.status.Index] = true
	}
	index := -1
	for _, candidate := range m.localReplicaIndexes(definition.Effective.Scaling.ReplicasMaximum) {
		if !used[candidate] {
			index = candidate
			break
		}
	}
	if index < 0 {
		m.mu.Unlock()
		return nil, executionservices.ErrReplicaCapacity
	}
	m.mu.Unlock()

	instance, err := m.prepareReplica(ctx, definition, index)
	if err != nil {
		m.mu.Lock()
		if current == m.services[definition.Identity.ServiceID()] {
			if len(current.instances) == 0 {
				current.status.State = StatePendingCapacity
			} else {
				current.status.State = StateDegraded
			}
			current.status.CapacityResource = capacityResource(err)
			current.status.CapacityReason = err.Error()
			status := cloneStatus(current.status)
			m.mu.Unlock()
			_ = m.writeObserved(status)
		} else {
			m.mu.Unlock()
		}
		return nil, fmt.Errorf("add service replica: %w", err)
	}
	m.mu.Lock()
	if current != m.services[definition.Identity.ServiceID()] {
		m.mu.Unlock()
		_ = m.pools.Stop(context.Background(), instance.status.PoolID)
		return nil, errors.New("service generation changed while adding capacity")
	}
	current.instances = append(current.instances, instance)
	sort.Slice(current.instances, func(i, j int) bool { return current.instances[i].status.Index < current.instances[j].status.Index })
	current.status.State = StateReady
	current.status.CapacityResource = ""
	current.status.CapacityReason = ""
	current.status.Instances = statusesOf(current.instances)
	current.status.InstanceCount = len(current.instances)
	current.status.WorkerCount = workerCount(current.instances)
	status := cloneStatus(current.status)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	return instance, nil
}

func (m *Manager) activateInstance(runtime *runtimeService, instance *runtimeInstance, workerIDs []string) *runtimeInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || instance == nil || m.services[runtime.status.ServiceID] != runtime || !slices.Contains(runtime.instances, instance) {
		return nil
	}
	instance.status.WorkerIDs = append([]string(nil), workerIDs...)
	instance.active++
	instance.status.ActiveRequests = instance.active
	runtime.status.Metrics.ActiveRequests++
	return instance
}

func capacityResource(err error) string {
	message := strings.ToLower(err.Error())
	for _, resource := range []string{"memory", "cpu", "worker", "sandbox", "pid", "storage", "network"} {
		if strings.Contains(message, resource) {
			return resource
		}
	}
	return "placement"
}

func (m *Manager) respondCapacityUnavailable(writer http.ResponseWriter, err error) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Retry-After", "1")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(map[string]string{
		"error":    "insufficient_capacity",
		"state":    string(StatePendingCapacity),
		"resource": capacityResource(err),
		"reason":   err.Error(),
	})
}

func (m *Manager) forwardAvailable(writer http.ResponseWriter, request *http.Request) (bool, error) {
	if m.nodes == nil {
		return false, nil
	}
	return m.nodes.ProxyAvailable(writer, request)
}

func persistentInstance(value *persistentDispatch) *runtimeInstance {
	if value == nil {
		return nil
	}
	return value.instance
}

func (m *Manager) beginPersistentDispatch(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition, request *http.Request, userID string, connect bool) (*persistentDispatch, error) {
	serviceID := definition.Identity.ServiceID()
	headerToken := strings.TrimSpace(request.Header.Get(RouteHeader))
	queryToken := ""
	if connect {
		queryToken = strings.TrimSpace(request.URL.Query().Get("route"))
	}
	if headerToken != "" && queryToken != "" && headerToken != queryToken {
		return nil, errors.New("persistent route header and query disagree")
	}
	token := headerToken
	if token == "" {
		token = queryToken
	}
	if token != "" {
		located, err := m.persistentRoutes.lookup(token, serviceID, userID)
		if err != nil {
			return nil, err
		}
		if located.NodeID != m.persistentRoutes.nodeID {
			return &persistentDispatch{token: token, record: located, remoteNode: located.NodeID}, nil
		}
		record, err := m.persistentRoutes.resolve(token, serviceID, userID, connect)
		if err != nil {
			return nil, err
		}
		instance := m.selectPersistentInstance(runtime, record.PoolID, record.WorkerID)
		if instance == nil {
			m.persistentRoutes.discard(token)
			return nil, errRouteNotFound
		}
		return &persistentDispatch{token: token, record: record, instance: instance}, nil
	}
	instance, err := m.selectCapacityInstance(ctx, runtime, definition)
	if err != nil {
		return nil, err
	}
	token, record, err := m.persistentRoutes.create(serviceID, instance.status.PoolID, instance.status.RuntimeGroupID, instance.status.SandboxID, userID, definition.Effective.Execution.KeepAlive, connect)
	if err != nil {
		m.finishRequest(runtime, instance, 0, 0, 0, false)
		return nil, err
	}
	return &persistentDispatch{token: token, record: record, instance: instance, initial: true}, nil
}

func (m *Manager) CompletePersistentExecution(_ context.Context, target callback.PersistentExecutionTarget) error {
	return m.persistentRoutes.complete(
		target.PersistentExecutionID,
		target.ServiceID,
		target.RuntimeGroupID,
		target.SandboxID,
		target.WorkerID,
	)
}

func (m *Manager) selectPersistentInstance(runtime *runtimeService, poolID, workerID string) *runtimeInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil {
		return nil
	}
	var selected *runtimeInstance
	for _, instance := range runtime.instances {
		if instance.status.PoolID == poolID {
			selected = instance
			break
		}
	}
	if selected == nil {
		selected = m.persistentInstances[poolID]
	}
	if selected == nil || workerID != "" && !slices.Contains(selected.status.WorkerIDs, workerID) {
		return nil
	}
	selected.active++
	selected.status.ActiveRequests = selected.active
	runtime.status.Metrics.ActiveRequests++
	return selected
}

func isWebSocketUpgrade(request *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") && headerHasToken(request.Header.Get("Connection"), "upgrade")
}

func headerHasToken(value, token string) bool {
	for _, item := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}

func (m *Manager) finishRequest(runtime *runtimeService, instance *runtimeInstance, status int, bytes uint64, duration time.Duration, timeout bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil {
		return
	}
	instance.active = max(instance.active-1, 0)
	instance.status.ActiveRequests = instance.active
	runtime.status.Metrics.ActiveRequests = max(runtime.status.Metrics.ActiveRequests-1, 0)
	runtime.status.Metrics.RequestCount++
	runtime.status.Metrics.RequestDuration = duration
	runtime.status.Metrics.BytesStreamed += bytes
	if timeout {
		runtime.status.Metrics.TimeoutCount++
	}
	if status != 0 {
		runtime.status.Metrics.ResponseStatus[strconv.Itoa(status)]++
	}
}

func (m *Manager) statusFromDefinition(definition workspacepackages.Definition, state State) Status {
	return Status{ServiceID: definition.Identity.ServiceID(), PackageID: definition.Identity.PackageID(), CanonicalBasePath: definition.Identity.CanonicalBasePath(), Description: definition.Service.Description, ExecutionMode: definition.Service.Execution.Mode, AccessMode: definition.Service.Access.Mode, Enabled: definition.State.Enabled, DesiredGeneration: definition.State.Generation, State: state, Entrypoint: definition.EntrypointPath, Effective: definition.Effective, Metrics: emptyMetrics()}
}

func (m *Manager) writeObserved(status Status) error {
	identity, err := workspacepackages.ParseServiceID(status.ServiceID)
	if err != nil {
		return err
	}
	directory := filepath.Join(m.observed, identity.Namespace, identity.Repository, identity.Service)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".status-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(directory, "status.json"))
}

func parseCanonicalRequest(value *url.URL) (workspacepackages.Identity, string, error) {
	if value == nil || !utf8.ValidString(value.Path) || strings.ContainsRune(value.Path, '\x00') || strings.Contains(value.Path, "\\") {
		return workspacepackages.Identity{}, "", errors.New("invalid URL path")
	}
	escaped := strings.ToLower(value.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(escaped, "%00") {
		return workspacepackages.Identity{}, "", errors.New("encoded separator or null byte")
	}
	parts := strings.Split(strings.TrimPrefix(value.Path, "/"), "/")
	if len(parts) < 3 {
		return workspacepackages.Identity{}, "", errNotServicePath
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return workspacepackages.Identity{}, "", errors.New("path traversal")
		}
	}
	identity, err := workspacepackages.ParseServiceID(strings.Join(parts[:3], "/"))
	if err != nil {
		return workspacepackages.Identity{}, "", err
	}
	relative := "/"
	if len(parts) > 3 {
		relative += strings.Join(parts[3:], "/")
	}
	return identity, relative, nil
}

var errNotServicePath = errors.New("not a canonical service path")

func originalURL(request *http.Request) string {
	scheme := requestScheme(request)
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	return scheme + "://" + host + request.URL.RequestURI()
}
func requestScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	forwarded := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0])
	if strings.EqualFold(forwarded, "https") {
		return "https"
	}
	return "http"
}
func cloneURL(value *url.URL) *url.URL { copied := *value; return &copied }
func generationPoolID(serviceID string, generation uint64, index int) string {
	return hashedID("service", fmt.Sprintf("%s\x00%d\x00%d", serviceID, generation, index))
}
func validationPoolID(serviceID string) string {
	id, _ := model.NewID("validation")
	return hashedID("validation", serviceID+"\x00"+id)
}
func hashedID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}
func coalesce(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func containsPool(instances []*runtimeInstance, poolID string) bool {
	for _, instance := range instances {
		if instance.status.PoolID == poolID {
			return true
		}
	}
	return false
}
func instancesOf(service *runtimeService) []*runtimeInstance {
	if service == nil {
		return nil
	}
	return service.instances
}
func statusesOf(instances []*runtimeInstance) []InstanceStatus {
	result := make([]InstanceStatus, 0, len(instances))
	for _, instance := range instances {
		status := instance.status
		status.WorkerIDs = append([]string{}, status.WorkerIDs...)
		result = append(result, status)
	}
	return result
}
func workerCount(instances []*runtimeInstance) int {
	total := 0
	for _, instance := range instances {
		total += len(instance.status.WorkerIDs)
	}
	return total
}
func emptyMetrics() Metrics { return Metrics{ResponseStatus: map[string]uint64{}} }
func cloneMetrics(value Metrics) Metrics {
	value.ResponseStatus = cloneMap(value.ResponseStatus)
	return value
}
func cloneStatus(value Status) Status {
	value.Instances = append([]InstanceStatus(nil), value.Instances...)
	for index := range value.Instances {
		value.Instances[index].WorkerIDs = append([]string{}, value.Instances[index].WorkerIDs...)
	}
	value.Metrics = cloneMetrics(value.Metrics)
	return value
}
func cloneMap(value map[string]uint64) map[string]uint64 {
	result := map[string]uint64{}
	for key, item := range value {
		result[key] = item
	}
	return result
}
func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
