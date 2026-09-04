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
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	"the8020/kernel/database"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/execution/supervisor"
	executionworkers "the8020/kernel/execution/workers"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/runtime/callback"
	"the8020/kernel/sandbox/model"
)

const internalHeaderPrefix = "X-80-20-Internal-"

// A dispatch reservation only bridges the interval before an absolute
// supervisor snapshot observes the request. It must not become durable truth
// when a transport disappears without completing its cleanup path.
const dispatchReservationLifetime = 30 * time.Second
const maximumServiceMaintenancePerPass = 256

type RuntimePools interface {
	Start(context.Context, string, string, executionservices.Options) (executionservices.Record, error)
	ListForService(string) ([]executionservices.Record, error)
	Inspect(string) (executionservices.Record, error)
	Capacity(context.Context, string) (executionservices.Record, error)
	Scale(context.Context, string, int) (executionservices.Record, error)
	EnsureCapacity(context.Context, string, int, int) (executionservices.Record, error)
	ReconcileCapacity(context.Context, string, int) (executionservices.Record, error)
	OpenAPI(context.Context, string) (map[string]any, error)
	Dispatch(context.Context, string, *http.Request) (*http.Response, error)
	ProxyWebSocket(context.Context, string, http.ResponseWriter, *http.Request) error
	Stop(context.Context, string) (bool, error)
	RemoveStopped(string) error
}

type BoundaryRouter interface {
	RegisterServiceBoundary(http.Handler) error
}

type Authentication interface {
	CookieName() string
	ValidateCookieContext(context.Context, string) (auth.AuthContext, error)
}

type RuntimeRequestRegistrar interface {
	BeginRuntimeRequest(auth.RuntimeRequest) (func(), error)
}

type NodeRouter interface {
	LocalNodeID() string
	LocalIndexes(int) []int
	OwnsIndex(int) bool
	Proxy(string, http.ResponseWriter, *http.Request) error
	ProxyAvailable(http.ResponseWriter, *http.Request) (bool, error)
}

type Config struct {
	Definitions       *workspacepackages.Store
	Pools             RuntimePools
	Router            BoundaryRouter
	ObservedRoot      string
	ReconcileInterval time.Duration
	StartupTimeout    time.Duration
	Logger            *slog.Logger
	Authentication    Authentication
	RuntimeRequests   RuntimeRequestRegistrar
	NodeID            string
	Database          database.Store
	Nodes             NodeRouter
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

type ServiceSandboxStatus struct {
	Index              int       `json:"index"`
	Version            uint64    `json:"version"`
	PoolID             string    `json:"pool_id"`
	RuntimeGroupID     string    `json:"runtime_group_id"`
	SandboxID          string    `json:"sandbox_id"`
	WorkerIDs          []string  `json:"worker_ids"`
	ActiveRequests     int       `json:"active_requests"`
	ActiveExecutions   int       `json:"active_executions"`
	SnapshotRevision   uint64    `json:"snapshot_revision,omitempty"`
	SnapshotObservedAt time.Time `json:"snapshot_observed_at,omitempty"`
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
	ServiceType       string                                   `json:"service_type,omitempty"`
	AccessMode        string                                   `json:"access_mode,omitempty"`
	Enabled           bool                                     `json:"enabled"`
	DesiredVersion    uint64                                   `json:"desired_version"`
	LoadedVersion     uint64                                   `json:"loaded_version"`
	VersionCount      int                                      `json:"version_count"`
	State             State                                    `json:"state"`
	SandboxCount      int                                      `json:"sandbox_count"`
	WorkerCount       int                                      `json:"worker_count"`
	Sandboxes         []ServiceSandboxStatus                   `json:"sandboxes"`
	Entrypoint        string                                   `json:"source_entrypoint,omitempty"`
	Effective         workspacepackages.EffectiveConfiguration `json:"effective_configuration"`
	ValidationError   string                                   `json:"validation_error,omitempty"`
	LastStartupError  string                                   `json:"last_startup_error,omitempty"`
	CapacityResource  string                                   `json:"capacity_resource,omitempty"`
	CapacityReason    string                                   `json:"capacity_reason,omitempty"`
	LastRestartTime   time.Time                                `json:"last_restart_time,omitempty"`
	FailedVersion     uint64                                   `json:"failed_version,omitempty"`
	Metrics           Metrics                                  `json:"metrics"`
}

type ValidationResult struct {
	ServiceID string         `json:"service_id"`
	Valid     bool           `json:"valid"`
	OpenAPI   map[string]any `json:"openapi,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type ScaleOptions struct {
	MinimumWorkers       *int
	MaximumWorkers       *int
	ConcurrencyPerWorker *int
	TargetUtilization    *float64
	WorkerKeepAlive      *string
	WorkersPerSandbox    *int
	SandboxGroup         *string
	MinimumSandboxes     *int
	ServiceType          *string
	SessionKeepAlive     *string
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

type runtimeSandbox struct {
	status       ServiceSandboxStatus
	reservations []time.Time
}

type runtimeService struct {
	status          Status
	sandboxes       []*runtimeSandbox
	retired         []*runtimeSandbox
	metrics         *Metrics
	definition      workspacepackages.Definition
	rejected        bool
	rejectedVersion uint64
}

type persistentDispatch struct {
	token      string
	record     persistentRoute
	sandbox    *runtimeSandbox
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

	mu               sync.Mutex
	services         map[string]*runtimeService
	persistentRoutes *persistentRouteRegistry
	capacityLocks    sync.Map
	maintenanceMu    sync.Mutex
	maintenanceQueue []string
	maintenanceHead  int
	maintenanceSet   map[string]bool
	background       context.Context
	stopBackground   context.CancelFunc
	cancel           context.CancelFunc
	wait             sync.WaitGroup
}

func New(config Config) (*Manager, error) {
	if config.Definitions == nil || config.Pools == nil || config.Router == nil || config.Database == nil {
		return nil, errors.New("definition store, runtime pools, service boundary router, and database are required")
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
	background, stopBackground := context.WithCancel(context.Background())
	manager := &Manager{definitions: config.Definitions, pools: config.Pools, observed: config.ObservedRoot, interval: config.ReconcileInterval, startup: config.StartupTimeout, logger: config.Logger, authentication: config.Authentication, runtimeRequests: config.RuntimeRequests, nodes: config.Nodes, services: map[string]*runtimeService{}, persistentRoutes: newPersistentRouteRegistry(config.NodeID, config.Database), background: background, stopBackground: stopBackground, maintenanceSet: map[string]bool{}}
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
			m.logger.Error("initial service reconciliation failed", "error", err)
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
	stopBackground := m.stopBackground
	m.cancel = nil
	m.stopBackground = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stopBackground != nil {
		stopBackground()
	}
	m.wait.Wait()
	return nil
}

func (m *Manager) ReconcileAll(ctx context.Context) error {
	return m.reconcileAll(ctx, true)
}

func (m *Manager) reconcileAll(ctx context.Context, provision bool) error {
	ids, err := m.definitions.ListStateServiceIDs()
	if err != nil {
		return err
	}
	var joined error
	for _, serviceID := range ids {
		if ctx.Err() != nil {
			return errors.Join(joined, ctx.Err())
		}
		if _, err := m.reconcileService(ctx, serviceID, provision); err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", serviceID, err))
		}
	}
	return joined
}

func (m *Manager) reconcileMaintained(ctx context.Context) error {
	var joined error
	ids := m.takeMaintenance(maximumServiceMaintenancePerPass)
	for _, serviceID := range ids {
		if ctx.Err() != nil {
			return errors.Join(joined, ctx.Err())
		}
		if _, err := m.reconcileService(ctx, serviceID, false); err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", serviceID, err))
		}
	}
	return joined
}

func (m *Manager) scheduleMaintenance(serviceID string) {
	if serviceID == "" {
		return
	}
	m.maintenanceMu.Lock()
	if !m.maintenanceSet[serviceID] {
		m.maintenanceSet[serviceID] = true
		m.maintenanceQueue = append(m.maintenanceQueue, serviceID)
	}
	m.maintenanceMu.Unlock()
}

func (m *Manager) scheduleMaintenanceIfNeeded(serviceID string) {
	m.mu.Lock()
	runtime := m.services[serviceID]
	needed := runtime != nil && (len(runtime.sandboxes) > 0 || len(runtime.retired) > 0 || runtime.status.State == StatePendingCapacity || runtime.status.State == StateDraining)
	m.mu.Unlock()
	if needed {
		m.scheduleMaintenance(serviceID)
	}
}

func (m *Manager) takeMaintenance(limit int) []string {
	if limit < 1 {
		return nil
	}
	m.maintenanceMu.Lock()
	available := len(m.maintenanceQueue) - m.maintenanceHead
	count := min(limit, available)
	result := append([]string(nil), m.maintenanceQueue[m.maintenanceHead:m.maintenanceHead+count]...)
	m.maintenanceHead += count
	for _, serviceID := range result {
		delete(m.maintenanceSet, serviceID)
	}
	if m.maintenanceHead == len(m.maintenanceQueue) {
		m.maintenanceQueue = nil
		m.maintenanceHead = 0
	} else if m.maintenanceHead >= 1024 && m.maintenanceHead*2 >= len(m.maintenanceQueue) {
		m.maintenanceQueue = append([]string(nil), m.maintenanceQueue[m.maintenanceHead:]...)
		m.maintenanceHead = 0
	}
	m.maintenanceMu.Unlock()
	return result
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

// Reload increments the service version without changing whether the
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

// Reconcile applies the current database-authoritative version without
// changing desired state. Package publication and cross-node revision
// convergence use this idempotent path after the version row is committed.
func (m *Manager) Reconcile(ctx context.Context, serviceID string) (Status, error) {
	if _, err := m.definitions.ReadService(serviceID); err != nil {
		return Status{}, err
	}
	return m.reconcileOne(ctx, serviceID)
}

// Retire removes runtime capacity for a service whose package no longer
// declares it. Shared desired state remains available if a later package
// version restores the service.
func (m *Manager) Retire(ctx context.Context, serviceID string) error {
	if _, err := workspacepackages.ParseServiceID(serviceID); err != nil {
		return err
	}
	unlockCapacity := m.lockServiceCapacity(serviceID)
	defer unlockCapacity()
	records, err := m.pools.ListForService(serviceID)
	if err != nil {
		return err
	}
	poolIDs := map[string]bool{}
	for _, record := range records {
		if record.LogicalServiceID == serviceID {
			poolIDs[record.ServiceID] = true
		}
	}
	m.mu.Lock()
	delete(m.services, serviceID)
	m.mu.Unlock()
	m.persistentRoutes.discardService(serviceID)
	var joined error
	for poolID := range poolIDs {
		stopped, stopErr := m.pools.Stop(ctx, poolID)
		if stopErr != nil && !errors.Is(stopErr, os.ErrNotExist) {
			joined = errors.Join(joined, stopErr)
			continue
		}
		if !stopped {
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
		if options.MinimumWorkers != nil {
			state.Scaling.MinimumWorkers = copyInt(options.MinimumWorkers)
		}
		if options.MaximumWorkers != nil {
			state.Scaling.MaximumWorkers = copyInt(options.MaximumWorkers)
		}
		if options.ConcurrencyPerWorker != nil {
			state.Scaling.ConcurrencyPerWorker = copyInt(options.ConcurrencyPerWorker)
		}
		if options.TargetUtilization != nil {
			value := *options.TargetUtilization
			state.Scaling.TargetUtilization = &value
		}
		if options.WorkerKeepAlive != nil {
			value := *options.WorkerKeepAlive
			state.Scaling.WorkerKeepAlive = &value
		}
		if options.SandboxGroup != nil {
			value := *options.SandboxGroup
			state.Placement.SandboxGroup = &value
		}
		if options.MinimumSandboxes != nil {
			state.Placement.MinimumSandboxes = copyInt(options.MinimumSandboxes)
		}
		if options.WorkersPerSandbox != nil {
			state.Placement.WorkersPerSandbox = copyInt(options.WorkersPerSandbox)
		}
		if options.ServiceType != nil {
			value := *options.ServiceType
			state.Lifecycle.ServiceType = &value
		}
		if options.SessionKeepAlive != nil {
			value := *options.SessionKeepAlive
			state.Lifecycle.SessionKeepAlive = &value
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
	m.logger.Info("service desired state changed", "action", action, "package_id", identity.PackageID(), "service_id", serviceID, "enabled", state.Enabled, "desired_version", state.Generation)
}

func copyInt(value *int) *int { copied := *value; return &copied }

func (m *Manager) reconcileOne(ctx context.Context, serviceID string) (Status, error) {
	status, err := m.reconcileService(ctx, serviceID, true)
	m.mu.Lock()
	if runtime := m.services[serviceID]; runtime != nil {
		status = m.observedStatusLocked(cloneRuntimeStatus(runtime), observedSandboxesOf(runtime))
	}
	m.mu.Unlock()
	return status, err
}

func (m *Manager) reconcileService(ctx context.Context, serviceID string, provision bool) (Status, error) {
	unlockCapacity := m.lockServiceCapacity(serviceID)
	defer unlockCapacity()
	defer m.scheduleMaintenanceIfNeeded(serviceID)
	definition, err := m.definitions.ReadService(serviceID)
	if err != nil {
		m.mu.Lock()
		existing := m.services[serviceID]
		if errors.Is(err, workspacepackages.ErrPackageNotReady) && existing != nil {
			status := cloneRuntimeStatus(existing)
			m.mu.Unlock()
			return status, nil
		}
		m.mu.Unlock()
		return m.retainFailedVersion(serviceID, 0, err)
	}
	if !definition.StateExists {
		return m.statusFromDefinition(definition, StateDiscovered), nil
	}
	if !definition.State.Enabled {
		return m.stopRuntime(ctx, definition)
	}
	m.mu.Lock()
	existing := m.services[serviceID]
	previous := append([]*runtimeSandbox(nil), sandboxesOf(existing)...)
	var retired []*runtimeSandbox
	if existing != nil {
		retired = append(retired, existing.retired...)
	}
	sameVersion := existing != nil && existing.status.LoadedVersion == definition.State.Generation && (existing.status.State == StateReady || existing.status.State == StateDegraded || existing.status.State == StateIdle)
	rejectedVersion := existing != nil && existing.rejected && existing.rejectedVersion == definition.State.Generation
	failedVersion := existing != nil && existing.status.State == StateFailed && existing.status.DesiredVersion == definition.State.Generation
	existingStatus := cloneRuntimeStatus(existing)
	m.mu.Unlock()
	if !provision && rejectedVersion {
		return existingStatus, nil
	}
	if !provision && len(previous) == 0 {
		if failedVersion {
			return existingStatus, nil
		}
		requiresCapacity := definition.Effective.Scaling.MinimumWorkers > 0 || definition.Effective.Placement.MinimumSandboxes > 0
		if !requiresCapacity && (existing == nil || existing.status.State != StatePendingCapacity) {
			idle := m.statusFromDefinition(definition, StateIdle)
			idle.LoadedVersion, idle.VersionCount = definition.State.Generation, 1
			m.mu.Lock()
			m.services[serviceID] = replacementRuntime(existing, idle, nil, retired, definition)
			m.mu.Unlock()
			_ = m.writeObserved(idle)
			cleanupErr := m.cleanupStaleVersionPools(ctx, definition, nil)
			if cleanupErr != nil {
				m.logVersionCleanupFailure(serviceID, cleanupErr)
			}
			return cloneStatus(idle), cleanupErr
		}
	}
	if sameVersion {
		status, healthy, refreshErr := m.refreshVersion(ctx, definition, existing, previous)
		if refreshErr != nil {
			return m.retainFailedVersion(serviceID, definition.State.Generation, refreshErr)
		}
		if healthy {
			m.mu.Lock()
			active := append([]*runtimeSandbox(nil), sandboxesOf(existing)...)
			m.mu.Unlock()
			cleanupErr := m.cleanupStaleVersionPools(ctx, definition, active)
			if cleanupErr != nil {
				m.logVersionCleanupFailure(serviceID, cleanupErr)
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
		starting.LoadedVersion = existing.status.LoadedVersion
		starting.VersionCount = existing.status.VersionCount
	}
	starting.Sandboxes = statusesOf(previous)
	starting.SandboxCount = len(previous)
	starting.WorkerCount = workerCount(previous)
	m.mu.Lock()
	retainedDefinition := definition
	if existing != nil && len(previous) > 0 {
		retainedDefinition = existing.definition
	}
	m.services[serviceID] = replacementRuntime(existing, starting, previous, retired, retainedDefinition)
	m.mu.Unlock()
	_ = m.writeObserved(starting)
	if m.logger != nil {
		m.logger.Info("service version preparing", "package_id", definition.Identity.PackageID(), "service_id", serviceID, "desired_version", definition.State.Generation, "loaded_version", starting.LoadedVersion, "state", starting.State)
	}

	startupContext, cancel := context.WithTimeout(ctx, m.startup)
	defer cancel()
	prepared, preparationErrors := m.prepareVersion(startupContext, definition)
	preparationFailure := errors.Join(preparationErrors...)
	if len(preparationErrors) > 0 && errors.Is(preparationFailure, executionservices.ErrInvalidServiceDefinition) {
		previousPools := make(map[string]bool, len(previous))
		for _, sandbox := range previous {
			previousPools[sandbox.status.PoolID] = true
		}
		var cleanupErr error
		for _, sandbox := range prepared {
			if previousPools[sandbox.status.PoolID] {
				continue
			}
			stopped, err := m.pools.Stop(context.Background(), sandbox.status.PoolID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, err)
				continue
			}
			if stopped {
				if err := m.pools.RemoveStopped(sandbox.status.PoolID); err != nil && !errors.Is(err, os.ErrNotExist) {
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
		}
		cleanupErr = errors.Join(cleanupErr, m.cleanupStaleVersionPools(ctx, definition, previous))
		if cleanupErr != nil {
			m.logVersionCleanupFailure(serviceID, cleanupErr)
		}
		return m.retainRejectedVersion(serviceID, definition.State.Generation, errors.Join(preparationFailure, cleanupErr))
	}
	if len(preparationErrors) > 0 && len(previous) > 0 {
		previousPools := make(map[string]bool, len(previous))
		for _, sandbox := range previous {
			previousPools[sandbox.status.PoolID] = true
		}
		for _, sandbox := range prepared {
			if !previousPools[sandbox.status.PoolID] {
				_, _ = m.pools.Stop(context.Background(), sandbox.status.PoolID)
			}
		}
		return m.retainFailedVersion(serviceID, definition.State.Generation, errors.Join(preparationErrors...))
	}
	localMinimumSandboxes := len(m.minimumSandboxIndexes(definition))
	if len(prepared) < localMinimumSandboxes {
		if len(preparationErrors) == 0 {
			preparationErrors = append(preparationErrors, fmt.Errorf("ready local service sandboxes %d are below required minimum %d", len(prepared), localMinimumSandboxes))
		}
	}

	state := StateReady
	if workerCount(prepared) == 0 {
		state = StateIdle
	}
	failure := ""
	if len(preparationErrors) > 0 && len(prepared) == 0 && localMinimumSandboxes > 0 {
		state, failure = StatePendingCapacity, errors.Join(preparationErrors...).Error()
	} else if len(preparationErrors) > 0 {
		state, failure = StateDegraded, errors.Join(preparationErrors...).Error()
	}
	status := m.statusFromDefinition(definition, state)
	if len(prepared) > 0 || localMinimumSandboxes == 0 {
		status.LoadedVersion = definition.State.Generation
		status.VersionCount = 1
	}
	status.LastRestartTime = time.Now().UTC()
	status.LastStartupError = failure
	status.Sandboxes = statusesOf(prepared)
	status.SandboxCount, status.WorkerCount = len(prepared), workerCount(prepared)
	retired = append(retired, previous...)
	m.mu.Lock()
	metrics := runtimeMetrics(existing)
	if existing != nil && existing.status.VersionCount > 0 && existing.status.LoadedVersion != status.LoadedVersion {
		metrics.WorkerRestarts += uint64(workerCount(previous))
	}
	m.services[serviceID] = replacementRuntime(existing, status, prepared, retired, definition)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	cleanupErr := m.cleanupStaleVersionPools(ctx, definition, prepared)
	if cleanupErr != nil {
		m.logVersionCleanupFailure(serviceID, cleanupErr)
	}
	if m.logger != nil {
		m.logger.Info("service version ready", "service_id", serviceID, "desired_version", definition.State.Generation, "sandbox_count", len(prepared), "worker_count", status.WorkerCount, "state", status.State)
	}
	if len(preparationErrors) > 0 || cleanupErr != nil {
		return cloneStatus(status), errors.Join(errors.Join(preparationErrors...), cleanupErr)
	}
	return cloneStatus(status), nil
}

func (m *Manager) cleanupStaleVersionPools(ctx context.Context, definition workspacepackages.Definition, active []*runtimeSandbox) error {
	records, err := m.pools.ListForService(definition.Identity.ServiceID())
	if err != nil {
		return fmt.Errorf("list runtime pools for version cleanup: %w", err)
	}
	activeIDs := make(map[string]bool, len(active))
	for _, sandbox := range active {
		activeIDs[sandbox.status.PoolID] = true
	}
	var joined error
	var retained []executionservices.Record
	for _, record := range records {
		ownedRelease := strings.HasPrefix(record.ReleaseID, "service-version-") || record.ReleaseID == "service-validation"
		if record.LogicalServiceID != definition.Identity.ServiceID() || activeIDs[record.ServiceID] || !ownedRelease {
			continue
		}
		if record.State == "STOPPED" && len(record.WorkerIDs) == 0 {
			if removeErr := m.pools.RemoveStopped(record.ServiceID); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				joined = errors.Join(joined, fmt.Errorf("remove retired pool %s version %d: %w", record.ServiceID, record.Generation, removeErr))
				continue
			}
			continue
		}
		drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
		stopped, stopErr := m.pools.Stop(drainContext, record.ServiceID)
		cancel()
		if stopErr != nil {
			joined = errors.Join(joined, fmt.Errorf("retire pool %s version %d: %w", record.ServiceID, record.Generation, stopErr))
			if strings.HasPrefix(record.ReleaseID, "service-version-") {
				retained = append(retained, record)
			}
		} else if stopped {
			if removeErr := m.pools.RemoveStopped(record.ServiceID); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				joined = errors.Join(joined, fmt.Errorf("remove retired pool %s version %d: %w", record.ServiceID, record.Generation, removeErr))
				continue
			}
		} else if strings.HasPrefix(record.ReleaseID, "service-version-") {
			retained = append(retained, record)
		}
	}
	m.syncRetiredSandboxes(definition.Identity.ServiceID(), retained)
	return joined
}

func (m *Manager) syncRetiredSandboxes(serviceID string, records []executionservices.Record) {
	m.mu.Lock()
	runtime := m.services[serviceID]
	if runtime == nil {
		m.mu.Unlock()
		return
	}
	known := make(map[string]*runtimeSandbox, len(runtime.retired))
	for _, sandbox := range runtime.retired {
		known[sandbox.status.PoolID] = sandbox
	}
	retired := make([]*runtimeSandbox, 0, len(records))
	for _, record := range records {
		if record.SandboxID == "" && len(record.WorkerIDs) == 0 {
			continue
		}
		sandbox := known[record.ServiceID]
		if sandbox == nil {
			sandbox = &runtimeSandbox{}
		}
		sandbox.status.Index = record.SandboxIndex
		sandbox.status.Version = record.Generation
		sandbox.status.PoolID = record.ServiceID
		sandbox.status.RuntimeGroupID = record.RuntimeGroupID
		sandbox.status.SandboxID = record.SandboxID
		sandbox.status.WorkerIDs = append([]string(nil), record.WorkerIDs...)
		retired = append(retired, sandbox)
	}
	runtime.retired = retired
	status := m.observedStatusLocked(cloneRuntimeStatus(runtime), observedSandboxesOf(runtime))
	m.mu.Unlock()
	_ = m.writeObserved(status)
}

func (m *Manager) logVersionCleanupFailure(serviceID string, err error) {
	if m.logger != nil {
		m.logger.Error("service version cleanup failed", "service_id", serviceID, "error", err)
	}
}

func (m *Manager) refreshVersion(ctx context.Context, definition workspacepackages.Definition, service *runtimeService, sandboxes []*runtimeSandbox) (Status, bool, error) {
	minimumIndexes := m.minimumSandboxIndexes(definition)
	capacity := make(map[string]executionservices.Record, len(sandboxes))
	refreshed := make(map[*runtimeSandbox]ServiceSandboxStatus, len(sandboxes))
	for _, sandbox := range sandboxes {
		record, err := m.pools.Inspect(sandbox.status.PoolID)
		if err != nil || (record.State != "READY" && record.State != "IDLE") || record.Generation != definition.State.Generation {
			return Status{}, false, nil
		}
		status := sandbox.status
		status.Version = record.Generation
		status.RuntimeGroupID = record.RuntimeGroupID
		status.SandboxID = record.SandboxID
		status.WorkerIDs = append([]string{}, record.WorkerIDs...)
		refreshed[sandbox] = status
		capacity[record.ServiceID] = record
	}
	floorSandboxes := make([]*runtimeSandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		floorSandboxes = append(floorSandboxes, &runtimeSandbox{status: refreshed[sandbox]})
	}
	minimumWorkers := m.minimumWorkersBySandbox(definition, floorSandboxes)
	blocked := map[string]bool{}
	for _, sandbox := range sandboxes {
		record, err := m.pools.ReconcileCapacity(ctx, sandbox.status.PoolID, minimumWorkers[sandbox.status.Index])
		status := refreshed[sandbox]
		status.RuntimeGroupID = record.RuntimeGroupID
		status.SandboxID = record.SandboxID
		status.WorkerIDs = append([]string{}, record.WorkerIDs...)
		refreshed[sandbox] = status
		capacity[record.ServiceID] = record
		if err == nil {
			continue
		}
		if errors.Is(err, executionworkers.ErrSandboxCapacity) || errors.Is(err, executionservices.ErrSandboxCapacity) {
			blocked[sandbox.status.PoolID] = true
			continue
		}
		return Status{}, false, fmt.Errorf("reconcile Workers for service sandbox %d: %w", sandbox.status.Index, err)
	}

	present := make(map[int]bool, len(sandboxes))
	for _, sandbox := range sandboxes {
		sandbox.status = refreshed[sandbox]
		present[sandbox.status.Index] = true
	}
	var preparationErrors []error
	for _, index := range minimumIndexes {
		if present[index] {
			continue
		}
		initialWorkers := 0
		if workerCount(sandboxes) < len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers)) {
			initialWorkers = 1
		}
		sandbox, err := m.prepareSandbox(ctx, definition, index, initialWorkers, initialWorkers)
		if err != nil {
			preparationErrors = append(preparationErrors, fmt.Errorf("service sandbox %d: %w", index, err))
			continue
		}
		sandboxes = append(sandboxes, sandbox)
		present[index] = true
		if record, inspectErr := m.pools.Inspect(sandbox.status.PoolID); inspectErr == nil {
			capacity[record.ServiceID] = record
		}
	}
	var ensureErr error
	sandboxes, ensureErr = m.ensureMinimumWorkers(ctx, definition, sandboxes, blocked)
	if ensureErr != nil {
		preparationErrors = append(preparationErrors, fmt.Errorf("maintain minimum Workers: %w", ensureErr))
	}
	complete := containsSandboxIndexes(sandboxes, minimumIndexes) && workerCount(sandboxes) >= len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers))
	sort.Slice(sandboxes, func(i, j int) bool { return sandboxes[i].status.Index < sandboxes[j].status.Index })
	m.mu.Lock()
	current := m.services[definition.Identity.ServiceID()]
	if current != service {
		m.mu.Unlock()
		return Status{}, false, nil
	}
	current.sandboxes = sandboxes
	m.mu.Unlock()
	if complete {
		var err error
		minimumWorkers = m.minimumWorkersBySandbox(definition, sandboxes)
		sandboxes, err = m.scaleDownSandbox(ctx, definition, service, sandboxes, capacity, minimumWorkers)
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
	if !complete && len(sandboxes) > 0 {
		current.status.State = StateDegraded
	} else if !complete {
		current.status.State = StatePendingCapacity
	} else if workerCount(sandboxes) == 0 {
		current.status.State = StateIdle
	}
	current.status.Enabled = true
	current.status.DesiredVersion = definition.State.Generation
	current.status.CapacityResource = ""
	current.status.CapacityReason = ""
	if complete {
		current.status.FailedVersion = 0
		current.status.LastStartupError = ""
	}
	current.sandboxes = sandboxes
	current.status.Sandboxes = statusesOf(sandboxes)
	current.status.SandboxCount = len(sandboxes)
	current.status.WorkerCount = workerCount(sandboxes)
	status := cloneRuntimeStatus(current)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	if len(preparationErrors) > 0 {
		return status, true, errors.Join(preparationErrors...)
	}
	return status, true, nil
}

func containsSandboxIndexes(sandboxes []*runtimeSandbox, required []int) bool {
	present := make(map[int]bool, len(sandboxes))
	for _, sandbox := range sandboxes {
		present[sandbox.status.Index] = true
	}
	for _, index := range required {
		if !present[index] {
			return false
		}
	}
	return true
}

func (m *Manager) scaleDownSandbox(ctx context.Context, definition workspacepackages.Definition, service *runtimeService, sandboxes []*runtimeSandbox, capacity map[string]executionservices.Record, minimumWorkers map[int]int) ([]*runtimeSandbox, error) {
	candidates := make(map[*runtimeSandbox]bool)
	minimumIndexes := m.minimumSandboxIndexes(definition)
	for _, sandbox := range sandboxes {
		record, exists := capacity[sandbox.status.PoolID]
		m.mu.Lock()
		active := activeReservations(sandbox, time.Now())
		m.mu.Unlock()
		if active != 0 {
			continue
		}
		if isSessionService(definition) && m.persistentRoutes.hasPool(sandbox.status.PoolID) {
			continue
		}
		unowned := !m.ownsIndex(sandbox.status.Index)
		emptyExcess := minimumWorkers[sandbox.status.Index] == 0 && !slices.Contains(minimumIndexes, sandbox.status.Index) && exists && record.OccupiedSlots == 0 && len(record.WorkerIDs) == 0
		if unowned || emptyExcess {
			candidates[sandbox] = true
		}
	}
	if len(candidates) == 0 {
		return sandboxes, nil
	}

	serviceID := definition.Identity.ServiceID()
	m.mu.Lock()
	if m.services[serviceID] != service {
		m.mu.Unlock()
		return sandboxes, nil
	}
	remaining := make([]*runtimeSandbox, 0, len(service.sandboxes)-len(candidates))
	for _, sandbox := range service.sandboxes {
		if !candidates[sandbox] || activeReservations(sandbox, time.Now()) != 0 {
			remaining = append(remaining, sandbox)
			delete(candidates, sandbox)
		}
	}
	service.sandboxes = remaining
	m.mu.Unlock()

	var joined error
	for candidate := range candidates {
		drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
		_, err := m.pools.Stop(drainContext, candidate.status.PoolID)
		cancel()
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("remove service sandbox %d: %w", candidate.status.Index, err))
			m.mu.Lock()
			if m.services[serviceID] == service {
				service.sandboxes = append(service.sandboxes, candidate)
			}
			m.mu.Unlock()
			continue
		}
		if m.logger != nil {
			m.logger.Info("idle or unowned service sandbox removed", "service_id", serviceID, "sandbox_index", candidate.status.Index, "pool_id", candidate.status.PoolID, "sandbox_id", candidate.status.SandboxID)
		}
	}
	m.mu.Lock()
	result := append([]*runtimeSandbox(nil), service.sandboxes...)
	sort.Slice(service.sandboxes, func(i, j int) bool { return service.sandboxes[i].status.Index < service.sandboxes[j].status.Index })
	m.mu.Unlock()
	return result, joined
}

func (m *Manager) prepareVersion(ctx context.Context, definition workspacepackages.Definition) ([]*runtimeSandbox, []error) {
	indexes := m.minimumSandboxIndexes(definition)
	prepared := make([]*runtimeSandbox, 0, len(indexes))
	var failures []error
	remainingWorkers := len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers))
	for _, index := range indexes {
		initialWorkers := 0
		if remainingWorkers > 0 {
			initialWorkers = 1
			remainingWorkers--
		}
		sandbox, err := m.prepareSandbox(ctx, definition, index, initialWorkers, initialWorkers)
		if err != nil {
			failures = append(failures, fmt.Errorf("service sandbox %d: %w", index, err))
			continue
		}
		prepared = append(prepared, sandbox)
	}
	var err error
	prepared, err = m.ensureMinimumWorkers(ctx, definition, prepared, nil)
	if err != nil {
		failures = append(failures, err)
	}
	return prepared, failures
}

// ensureMinimumWorkers packs one Worker at a time into eligible service
// sandboxes. A typed sandbox-capacity rejection blocks only that candidate for
// this reconciliation; the next Worker is placed in a new compatible sandbox.
func (m *Manager) ensureMinimumWorkers(ctx context.Context, definition workspacepackages.Definition, sandboxes []*runtimeSandbox, blocked map[string]bool) ([]*runtimeSandbox, error) {
	minimum := len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers))
	if blocked == nil {
		blocked = map[string]bool{}
	}
	var placementErrors []error
	for workerCount(sandboxes) < minimum {
		sort.Slice(sandboxes, func(i, j int) bool { return sandboxes[i].status.Index < sandboxes[j].status.Index })
		grew := false
		for _, sandbox := range sandboxes {
			if blocked[sandbox.status.PoolID] || len(sandbox.status.WorkerIDs) >= definition.Effective.Placement.WorkersPerSandbox {
				continue
			}
			record, err := m.pools.Scale(ctx, sandbox.status.PoolID, len(sandbox.status.WorkerIDs)+1)
			if err == nil {
				sandbox.status.WorkerIDs = append([]string(nil), record.WorkerIDs...)
				grew = true
				break
			}
			if errors.Is(err, executionservices.ErrInvalidServiceDefinition) || errors.Is(err, executionworkers.ErrNodeCapacity) {
				return sandboxes, err
			}
			if !errors.Is(err, executionworkers.ErrSandboxCapacity) && !errors.Is(err, executionservices.ErrSandboxCapacity) {
				return sandboxes, err
			}
			blocked[sandbox.status.PoolID] = true
			placementErrors = append(placementErrors, fmt.Errorf("service sandbox %d: %w", sandbox.status.Index, err))
		}
		if grew {
			continue
		}
		used := make(map[int]bool, len(sandboxes))
		for _, sandbox := range sandboxes {
			used[sandbox.status.Index] = true
		}
		index := m.nextOwnedSandboxIndex(used)
		if index < 0 {
			return sandboxes, errors.Join(append(placementErrors, executionservices.ErrSandboxCapacity)...)
		}
		sandbox, err := m.prepareSandbox(ctx, definition, index, 1, 1)
		if err != nil {
			return sandboxes, errors.Join(append(placementErrors, fmt.Errorf("service sandbox %d: %w", index, err))...)
		}
		sandboxes = append(sandboxes, sandbox)
		placementErrors = nil
	}
	return sandboxes, nil
}

func (m *Manager) localIndexes(limit int) []int {
	if limit <= 0 {
		return nil
	}
	if m.nodes != nil {
		return m.nodes.LocalIndexes(limit)
	}
	result := make([]int, limit)
	for index := range result {
		result[index] = index
	}
	return result
}

func (m *Manager) ownsIndex(index int) bool {
	return index >= 0 && (m.nodes == nil || m.nodes.OwnsIndex(index))
}

func (m *Manager) minimumSandboxIndexes(definition workspacepackages.Definition) []int {
	indexes := append([]int(nil), m.localIndexes(definition.Effective.Placement.MinimumSandboxes)...)
	required := (len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers)) + definition.Effective.Placement.WorkersPerSandbox - 1) / definition.Effective.Placement.WorkersPerSandbox
	seen := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		seen[index] = true
	}
	for index := 0; len(indexes) < required; index++ {
		if !seen[index] && m.ownsIndex(index) {
			indexes = append(indexes, index)
			seen[index] = true
		}
	}
	sort.Ints(indexes)
	return indexes
}

func (m *Manager) minimumWorkersBySandbox(definition workspacepackages.Definition, sandboxes []*runtimeSandbox) map[int]int {
	result := make(map[int]int, len(sandboxes))
	ordered := append([]*runtimeSandbox(nil), sandboxes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].status.Index < ordered[j].status.Index })
	remaining := len(m.localIndexes(definition.Effective.Scaling.MinimumWorkers))
	for _, sandbox := range ordered {
		floor := min(len(sandbox.status.WorkerIDs), remaining)
		result[sandbox.status.Index] = floor
		remaining -= floor
	}
	return result
}

func (m *Manager) prepareSandbox(ctx context.Context, definition workspacepackages.Definition, index, minimumWorkers, initialWorkers int) (*runtimeSandbox, error) {
	poolID := versionPoolID(definition.Identity.ServiceID(), definition.State.Generation, index)
	record, err := m.pools.Start(ctx, poolID, definition.EntrypointURL, executionservices.Options{
		GroupKey:             definition.Effective.Placement.SandboxGroup,
		Namespace:            definition.Identity.Namespace,
		MinimumWorkers:       minimumWorkers,
		MaximumWorkers:       definition.Effective.Placement.WorkersPerSandbox,
		ConcurrencyPerWorker: definition.Effective.Scaling.ConcurrencyPerWorker,
		WorkerKeepAlive:      definition.Effective.Scaling.WorkerKeepAlive,
		ReleaseID:            fmt.Sprintf("service-version-%d", definition.State.Generation),
		LogicalServiceID:     definition.Identity.ServiceID(),
		Generation:           definition.State.Generation,
		CanonicalBasePath:    definition.Identity.CanonicalBasePath(),
		OpenAPI:              supervisor.OpenAPIMetadata{Title: definition.Service.OpenAPI.Title, Version: definition.Service.OpenAPI.Version, Description: coalesce(definition.Service.OpenAPI.Description, definition.Service.Description)},
		ValidateEntrypoint:   true,
		SandboxIndex:         index,
		DependencyMode:       runtimeDependencyMode(definition.Effective.DependencyMode),
		ExecutionMode:        serviceExecutionMode(definition),
		TargetUtilization:    definition.Effective.Scaling.TargetUtilization,
		PlacementWorkers:     initialWorkers,
	})
	acquired := err == nil
	if err != nil {
		if existing, inspectErr := m.pools.Inspect(poolID); inspectErr == nil && (existing.State == "READY" || existing.State == "IDLE") && existing.Generation == definition.State.Generation {
			record, err, acquired = existing, nil, true
		}
	}
	if err == nil && len(record.WorkerIDs) != initialWorkers {
		record, err = m.pools.Scale(ctx, poolID, initialWorkers)
	}
	if err == nil && len(record.WorkerIDs) > 0 {
		_, err = m.pools.OpenAPI(ctx, poolID)
	}
	if err != nil {
		if acquired {
			drainContext, cancel := context.WithTimeout(context.Background(), definition.Effective.Timeouts.Drain)
			stopped, stopErr := m.pools.Stop(drainContext, poolID)
			cancel()
			if stopped || errors.Is(stopErr, os.ErrNotExist) {
				removeErr := m.pools.RemoveStopped(poolID)
				if errors.Is(removeErr, os.ErrNotExist) {
					removeErr = nil
				}
				err = errors.Join(err, removeErr)
			} else {
				err = errors.Join(err, fmt.Errorf("rollback service sandbox pool: %w", stopErr))
			}
		}
		return nil, err
	}
	return &runtimeSandbox{status: ServiceSandboxStatus{Index: index, Version: record.Generation, PoolID: poolID, RuntimeGroupID: record.RuntimeGroupID, SandboxID: record.SandboxID, WorkerIDs: append([]string{}, record.WorkerIDs...)}}, nil
}

func (m *Manager) stopRuntime(ctx context.Context, definition workspacepackages.Definition) (Status, error) {
	m.mu.Lock()
	existing := m.services[definition.Identity.ServiceID()]
	sandboxes := append([]*runtimeSandbox(nil), sandboxesOf(existing)...)
	var retired []*runtimeSandbox
	if existing != nil {
		retired = append(retired, existing.retired...)
	}
	alreadyStopped := existing != nil && existing.status.State == StateStopped && !existing.status.Enabled && existing.status.DesiredVersion == definition.State.Generation && len(sandboxes) == 0 && len(retired) == 0
	status := m.statusFromDefinition(definition, StateDisabled)
	if existing != nil {
		status.LoadedVersion = existing.status.LoadedVersion
	}
	if len(sandboxes) > 0 || len(retired) > 0 {
		status.State = StateDraining
	}
	status.Sandboxes = statusesOf(sandboxes)
	status.SandboxCount, status.WorkerCount = len(sandboxes), workerCount(sandboxes)
	if len(sandboxes) > 0 {
		status.VersionCount = 1
	}
	m.services[definition.Identity.ServiceID()] = replacementRuntime(existing, status, sandboxes, retired, definition)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	var joined error
	remaining := make([]*runtimeSandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		drainContext, cancel := context.WithTimeout(ctx, definition.Effective.Timeouts.Drain)
		stopped, stopErr := m.pools.Stop(drainContext, sandbox.status.PoolID)
		cancel()
		joined = errors.Join(joined, stopErr)
		if stopErr == nil && !stopped {
			remaining = append(remaining, sandbox)
		}
	}
	joined = errors.Join(joined, m.cleanupStaleVersionPools(ctx, definition, remaining))
	m.mu.Lock()
	existing = m.services[definition.Identity.ServiceID()]
	retired = nil
	if existing != nil {
		retired = append(retired, existing.retired...)
	}
	m.mu.Unlock()
	if joined == nil && (len(remaining) > 0 || len(retired) > 0) {
		status.State = StateDraining
		status.Sandboxes = statusesOf(remaining)
		status.SandboxCount, status.WorkerCount, status.VersionCount = len(remaining), workerCount(remaining), 1
		m.mu.Lock()
		m.services[definition.Identity.ServiceID()] = replacementRuntime(existing, status, remaining, retired, definition)
		m.mu.Unlock()
		_ = m.writeObserved(status)
		return cloneStatus(status), nil
	}
	status.State, status.LoadedVersion = StateStopped, 0
	status.Sandboxes, status.SandboxCount, status.WorkerCount, status.VersionCount = nil, 0, 0, 0
	if joined != nil {
		status.State, status.LastStartupError = StateFailed, joined.Error()
	}
	m.mu.Lock()
	m.services[definition.Identity.ServiceID()] = replacementRuntime(existing, status, nil, retired, definition)
	m.mu.Unlock()
	_ = m.writeObserved(status)
	if m.logger != nil && (!alreadyStopped || joined != nil) {
		m.logger.Info("service stopped", "package_id", definition.Identity.PackageID(), "service_id", definition.Identity.ServiceID(), "desired_version", definition.State.Generation, "state", status.State)
	}
	return cloneStatus(status), joined
}

func (m *Manager) retainFailedVersion(serviceID string, version uint64, cause error) (Status, error) {
	m.mu.Lock()
	existing := m.services[serviceID]
	if existing == nil {
		status := Status{ServiceID: serviceID, State: StateFailed, Metrics: emptyMetrics()}
		existing = replacementRuntime(nil, status, nil, nil, workspacepackages.Definition{})
		m.services[serviceID] = existing
	}
	failureState := StateFailed
	if len(existing.sandboxes) > 0 || len(existing.retired) > 0 {
		failureState = StateDegraded
	}
	duplicate := existing.status.FailedVersion == version && existing.status.LastStartupError == cause.Error() && existing.status.State == failureState
	existing.status.State = failureState
	existing.status.DesiredVersion = version
	existing.status.FailedVersion = version
	existing.status.LastStartupError = cause.Error()
	if !duplicate {
		runtimeMetrics(existing).StartupFailures++
	}
	status := cloneRuntimeStatus(existing)
	m.mu.Unlock()
	if !duplicate {
		_ = m.writeObserved(status)
	}
	if m.logger != nil && !duplicate {
		m.logger.Error("service version failed", "service_id", serviceID, "desired_version", version, "loaded_version", status.LoadedVersion, "error", cause)
	}
	return status, cause
}

func (m *Manager) retainRejectedVersion(serviceID string, version uint64, cause error) (Status, error) {
	status, err := m.retainFailedVersion(serviceID, version, cause)
	m.mu.Lock()
	if current := m.services[serviceID]; current != nil && current.status.DesiredVersion == version {
		current.rejected = true
		current.rejectedVersion = version
		status = cloneRuntimeStatus(current)
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
		if errors.Is(err, workspacepackages.ErrPackageNotReady) {
			m.mu.Lock()
			existing := m.services[serviceID]
			if existing != nil {
				status := m.observedStatusLocked(cloneRuntimeStatus(existing), observedSandboxesOf(existing))
				m.mu.Unlock()
				return status, nil
			}
			m.mu.Unlock()
		}
		return Status{}, err
	}
	m.mu.Lock()
	existing := m.services[serviceID]
	if existing != nil {
		status := cloneRuntimeStatus(existing)
		status.Enabled, status.DesiredVersion = definition.State.Enabled, definition.State.Generation
		status.Description, status.Entrypoint, status.Effective = definition.Service.Description, definition.EntrypointPath, definition.Effective
		status = m.observedStatusLocked(status, observedSandboxesOf(existing))
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
		MinimumWorkers: 1, MaximumWorkers: 1, ConcurrencyPerWorker: 1,
		WorkerKeepAlive: definition.Effective.Scaling.WorkerKeepAlive,
		ReleaseID:       "service-validation", LogicalServiceID: serviceID,
		Generation: definition.State.Generation, CanonicalBasePath: definition.Identity.CanonicalBasePath(),
		OpenAPI:            supervisor.OpenAPIMetadata{Title: definition.Service.OpenAPI.Title, Version: definition.Service.OpenAPI.Version, Description: coalesce(definition.Service.OpenAPI.Description, definition.Service.Description)},
		ValidateEntrypoint: true,
		DependencyMode:     runtimeDependencyMode(definition.Effective.DependencyMode),
		ExecutionMode:      serviceExecutionMode(definition),
		TargetUtilization:  definition.Effective.Scaling.TargetUtilization,
		PlacementWorkers:   1,
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
	stopped, err := m.pools.Stop(context.Background(), poolID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if m.logger != nil {
			m.logger.Error("stop service validation pool", "service_pool_id", poolID, "error", err)
		}
		return
	}
	if !stopped {
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
	if existing != nil && len(existing.sandboxes) > 0 {
		poolID := existing.sandboxes[0].status.PoolID
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
	m.mu.Lock()
	runtime := m.services[serviceID]
	hasCapacity := runtime != nil && len(runtime.sandboxes) > 0
	definition := workspacepackages.Definition{}
	loadedDefinition := hasCapacity && runtime.definition.Identity.Service != ""
	if loadedDefinition {
		definition = runtime.definition
	}
	m.mu.Unlock()
	var definitionErr error
	if !loadedDefinition {
		definition, definitionErr = m.definitions.ReadService(serviceID)
	}
	m.mu.Lock()
	if !loadedDefinition {
		runtime = m.services[serviceID]
		hasCapacity = runtime != nil && len(runtime.sandboxes) > 0
	}
	rejectedVersion := runtime != nil && runtime.rejected && runtime.rejectedVersion == definition.State.Generation
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
		if rejectedVersion {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		_, reconcileErr := m.reconcileOne(request.Context(), serviceID)
		m.mu.Lock()
		runtime = m.services[serviceID]
		hasCapacity = runtime != nil && len(runtime.sandboxes) > 0
		m.mu.Unlock()
		if reconcileErr != nil && runtime == nil {
			if m.logger != nil {
				m.logger.Error("service cold start failed", "service_id", serviceID, "error", reconcileErr)
			}
			if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
				http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if reconcileErr != nil && m.logger != nil {
			m.logger.Warn("service cold start continuing with degraded capacity", "service_id", serviceID, "error", reconcileErr, "sandbox_count", len(runtime.sandboxes))
		}
		// A successfully loaded zero-minimum service intentionally has no
		// sandbox allocation yet. Request-time placement below creates its
		// first Worker and sandbox on demand.
	}
	if isSessionService(definition) {
		route, routeErr := m.beginPersistentDispatch(request.Context(), runtime, definition, request, authContext.UserID, isWebSocketUpgrade(request))
		if routeErr != nil {
			if errors.Is(routeErr, executionservices.ErrSandboxCapacity) {
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
			m.dispatch(writer, request, identity, relativePath, runtime, route.sandbox, definition.Effective.Timeouts.Request, authContext, route)
		}
		return
	}
	sandbox, capacityErr := m.selectCapacitySandbox(request.Context(), runtime, definition)
	if capacityErr != nil {
		if forwarded, _ := m.forwardAvailable(writer, request); !forwarded {
			m.respondCapacityUnavailable(writer, capacityErr)
		}
		return
	}
	if isWebSocketUpgrade(request) {
		m.dispatchRequestWebSocket(writer, request, identity, relativePath, runtime, sandbox, authContext)
		return
	}
	m.dispatch(writer, request, identity, relativePath, runtime, sandbox, definition.Effective.Timeouts.Request, authContext, nil)
}

func (m *Manager) dispatch(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, relativePath string, runtime *runtimeService, sandbox *runtimeSandbox, timeout time.Duration, authContext auth.AuthContext, persistent *persistentDispatch) {
	if sandbox == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	requestID, err := model.NewID("request")
	if err != nil {
		m.finishRequest(runtime, sandbox, 0, 0, 0, false)
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
	loadedVersion := runtime.status.LoadedVersion
	m.mu.Unlock()
	forwarded.Header.Set(internalHeaderPrefix+"Service-Generation", strconv.FormatUint(loadedVersion, 10))
	forwarded.Header.Set(internalHeaderPrefix+"Canonical-Base-Path", identity.CanonicalBasePath())
	forwarded.Header.Set(internalHeaderPrefix+"Original-URL", originalURL(request))
	forwarded.Header.Set(internalHeaderPrefix+"Original-Path", request.URL.RequestURI())
	forwarded.Header.Set(internalHeaderPrefix+"Original-Host", request.Host)
	forwarded.Header.Set(internalHeaderPrefix+"Original-Scheme", requestScheme(request))
	setClientMetadata(forwarded.Header, request.RemoteAddr)
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
			RequestID: requestID, ServiceID: identity.ServiceID(), RuntimeGroupID: sandbox.status.RuntimeGroupID,
			SandboxID: sandbox.status.SandboxID, Auth: authContext, SecureTransport: requestScheme(request) == "https",
		})
		if registrationErr != nil {
			if persistent != nil && persistent.initial {
				m.persistentRoutes.discard(persistent.token)
			}
			m.finishRequest(runtime, sandbox, 0, 0, time.Since(started), false)
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		defer release()
	}
	response, dispatchErr := m.pools.Dispatch(ctx, sandbox.status.PoolID, forwarded)
	if dispatchErr != nil {
		if persistent != nil && persistent.initial {
			m.persistentRoutes.discard(persistent.token)
		}
		m.finishRequest(runtime, sandbox, 0, 0, time.Since(started), errors.Is(ctx.Err(), context.DeadlineExceeded))
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
	m.finishRequest(runtime, sandbox, response.StatusCode, uint64(max64(written, 0)), duration, errors.Is(ctx.Err(), context.DeadlineExceeded))
	if m.logger != nil {
		m.logger.Info("service request completed", "package_id", identity.PackageID(), "service_id", identity.ServiceID(), "request_id", requestID, "runtime_group_id", sandbox.status.RuntimeGroupID, "sandbox_id", sandbox.status.SandboxID, "duration", duration, "status_code", response.StatusCode, "bytes_streamed", max64(written, 0))
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
	succeeded := m.dispatchWebSocket(writer, request, identity, runtime, persistent.sandbox, relativePath, authContext, persistent)
	m.persistentRoutes.disconnect(persistent.token, succeeded)
	if !succeeded && persistent.initial {
		m.persistentRoutes.discard(persistent.token)
	}
	m.finishRequest(runtime, persistent.sandbox, http.StatusSwitchingProtocols, 0, time.Since(started), false)
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

func (m *Manager) dispatchRequestWebSocket(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, relativePath string, runtime *runtimeService, sandbox *runtimeSandbox, authContext auth.AuthContext) {
	started := time.Now()
	defer func() {
		m.finishRequest(runtime, sandbox, http.StatusSwitchingProtocols, 0, time.Since(started), false)
	}()
	m.dispatchWebSocket(writer, request, identity, runtime, sandbox, relativePath, authContext, nil)
}

func (m *Manager) dispatchWebSocket(writer http.ResponseWriter, request *http.Request, identity workspacepackages.Identity, runtime *runtimeService, sandbox *runtimeSandbox, relativePath string, authContext auth.AuthContext, persistent *persistentDispatch) bool {
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
	loadedVersion := runtime.status.LoadedVersion
	m.mu.Unlock()
	forwarded.Header.Set(internalHeaderPrefix+"Service-Generation", strconv.FormatUint(loadedVersion, 10))
	forwarded.Header.Set(internalHeaderPrefix+"Canonical-Base-Path", identity.CanonicalBasePath())
	forwarded.Header.Set(internalHeaderPrefix+"Original-URL", originalURL(request))
	forwarded.Header.Set(internalHeaderPrefix+"Original-Path", request.URL.RequestURI())
	forwarded.Header.Set(internalHeaderPrefix+"Original-Host", request.Host)
	forwarded.Header.Set(internalHeaderPrefix+"Original-Scheme", requestScheme(request))
	setClientMetadata(forwarded.Header, request.RemoteAddr)
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
			RequestID: requestID, ServiceID: identity.ServiceID(), RuntimeGroupID: sandbox.status.RuntimeGroupID,
			SandboxID: sandbox.status.SandboxID, Auth: authContext, SecureTransport: requestScheme(request) == "https",
		})
		if registrationErr != nil {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return false
		}
		defer release()
	}
	if err := m.pools.ProxyWebSocket(request.Context(), sandbox.status.PoolID, writer, forwarded); err != nil {
		if m.logger != nil {
			m.logger.Error("service WebSocket proxy failed", "service_id", identity.ServiceID(), "request_id", requestID, "pool_id", sandbox.status.PoolID, "error", err)
		}
		http.Error(writer, "service proxy failed", http.StatusBadGateway)
		return false
	}
	return true
}

func setClientMetadata(header http.Header, remoteAddress string) {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	address, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return
	}
	address = address.Unmap()
	header.Set(internalHeaderPrefix+"Client-IP-Address", address.String())
	header.Set(internalHeaderPrefix+"Client-Network-Scope", clientNetworkScope(address))
}

func clientNetworkScope(address netip.Addr) string {
	switch {
	case address.IsLoopback():
		return "loopback"
	case address.IsPrivate():
		return "private"
	case address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast():
		return "link_local"
	case address.IsUnspecified() || address.IsMulticast():
		return "special"
	default:
		return "public"
	}
}

func (m *Manager) authenticate(request *http.Request) (auth.AuthContext, bool) {
	if m.authentication == nil {
		return auth.AuthContext{}, false
	}
	cookie, err := request.Cookie(m.authentication.CookieName())
	if err != nil || cookie.Value == "" {
		return auth.AuthContext{}, false
	}
	contextValue, err := m.authentication.ValidateCookieContext(request.Context(), cookie.Value)
	if err != nil {
		if m.logger != nil && !errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrSessionExpired) {
			m.logger.Warn("authentication validation failed", "error", err)
		}
		return auth.AuthContext{}, false
	}
	return contextValue, contextValue.Authenticated
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

func (m *Manager) selectSandbox(runtime *runtimeService) *runtimeSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || len(runtime.sandboxes) == 0 {
		return nil
	}
	sandboxes := append([]*runtimeSandbox(nil), runtime.sandboxes...)
	now := time.Now()
	sort.Slice(sandboxes, func(i, j int) bool {
		left, right := activeReservations(sandboxes[i], now), activeReservations(sandboxes[j], now)
		return left < right || (left == right && sandboxes[i].status.Index < sandboxes[j].status.Index)
	})
	selected := sandboxes[0]
	reserveRequest(selected, now)
	runtimeMetrics(runtime).ActiveRequests++
	return selected
}

func (m *Manager) selectCapacitySandbox(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeSandbox, error) {
	if sandbox, shouldScale := m.reserveCachedCapacity(ctx, runtime, definition); sandbox != nil {
		if shouldScale {
			m.triggerCapacityReconcile(runtime, definition)
		}
		return sandbox, nil
	}
	return m.selectCapacitySandboxSlow(ctx, runtime, definition)
}

// reserveCachedCapacity is the normal warm path: one short in-memory routing
// reservation and no Worker listing, reconciliation, or external I/O.
func (m *Manager) reserveCachedCapacity(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeSandbox, bool) {
	type candidate struct {
		sandbox *runtimeSandbox
		active  int
	}
	m.mu.Lock()
	if runtime == nil || m.services[definition.Identity.ServiceID()] != runtime {
		m.mu.Unlock()
		return nil, false
	}
	candidates := make([]candidate, 0, len(runtime.sandboxes))
	now := time.Now()
	for _, sandbox := range runtime.sandboxes {
		if sandbox.status.Version == runtime.status.LoadedVersion {
			candidates = append(candidates, candidate{sandbox: sandbox, active: activeReservations(sandbox, now)})
		}
	}
	m.mu.Unlock()

	concurrency := definition.Effective.Scaling.ConcurrencyPerWorker
	target := definition.Effective.Scaling.TargetUtilization
	var selected *runtimeSandbox
	selectedLoad := 0.0
	totalWorkers := 0
	selectedObserved := 0
	selectedWorkers := []string(nil)
	for _, candidate := range candidates {
		observed, err := m.pools.Capacity(ctx, candidate.sandbox.status.PoolID)
		if err != nil {
			continue
		}
		workers := len(observed.WorkerIDs)
		totalWorkers += workers
		if workers == 0 {
			continue
		}
		occupied := max(candidate.active, observed.OccupiedSlots)
		limit := workers * concurrency
		if concurrency > 1 {
			// Higher concurrency is a balancing target, with one bounded extra
			// request per Worker while targeted scale-up catches up.
			limit += workers
		}
		if occupied >= limit {
			continue
		}
		load := float64(occupied) / float64(workers*concurrency)
		if selected == nil || load < selectedLoad || (load == selectedLoad && candidate.sandbox.status.Index < selected.status.Index) {
			selected, selectedLoad = candidate.sandbox, load
			selectedObserved = observed.OccupiedSlots
			selectedWorkers = observed.WorkerIDs
		}
	}
	if selected == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.services[definition.Identity.ServiceID()] != runtime || !slices.Contains(runtime.sandboxes, selected) || selected.status.Version != runtime.status.LoadedVersion {
		return nil, false
	}
	workers := len(selectedWorkers)
	occupied := max(activeReservations(selected, time.Now()), selectedObserved)
	limit := workers * concurrency
	if concurrency > 1 {
		limit += workers
	}
	if workers == 0 || occupied >= limit {
		return nil, false
	}
	selected.status.WorkerIDs = append([]string(nil), selectedWorkers...)
	reserveRequest(selected, time.Now())
	runtimeMetrics(runtime).ActiveRequests++
	projected := float64(max(activeReservations(selected, time.Now()), selectedObserved+1)) / float64(workers*concurrency)
	maximum := definition.Effective.Scaling.MaximumWorkers
	shouldScale := projected >= target && (maximum == 0 || totalWorkers < maximum)
	return selected, shouldScale
}

func (m *Manager) selectCapacitySandboxSlow(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeSandbox, error) {
	unlockCapacity := m.lockServiceCapacity(definition.Identity.ServiceID())
	defer unlockCapacity()
	return m.selectCapacitySandboxLocked(ctx, runtime, definition, true)
}

func (m *Manager) selectCapacitySandboxLocked(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition, reserve bool) (*runtimeSandbox, error) {
	type candidate struct {
		sandbox *runtimeSandbox
		active  int
		workers int
	}
	m.mu.Lock()
	var sandboxes []*runtimeSandbox
	now := time.Now()
	for _, sandbox := range sandboxesOf(runtime) {
		if sandbox.status.Version == runtime.status.LoadedVersion {
			sandboxes = append(sandboxes, sandbox)
		}
	}
	totalWorkers := workerCount(sandboxes)
	candidates := make([]candidate, len(sandboxes))
	for index, sandbox := range sandboxes {
		candidates[index] = candidate{sandbox: sandbox, active: activeReservations(sandbox, now), workers: len(sandbox.status.WorkerIDs)}
	}
	m.mu.Unlock()
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].active < candidates[j].active || (candidates[i].active == candidates[j].active && candidates[i].sandbox.status.Index < candidates[j].sandbox.status.Index)
	})
	var fallback *runtimeSandbox
	var lastErr error
	for _, candidate := range candidates {
		sandbox := candidate.sandbox
		growthLimit := definition.Effective.Placement.WorkersPerSandbox
		if localMaximum := m.localWorkerMaximum(definition); localMaximum > 0 {
			growthLimit = min(growthLimit, candidate.workers+max(localMaximum-totalWorkers, 0))
		}
		record, err := m.pools.EnsureCapacity(ctx, sandbox.status.PoolID, growthLimit, candidate.active)
		if err == nil {
			if reserve {
				if activated := m.activateSandbox(runtime, sandbox, record.WorkerIDs); activated != nil {
					return activated, nil
				}
				return nil, errors.New("service version changed while selecting capacity")
			}
			return m.updateSandboxWorkers(runtime, sandbox, record.WorkerIDs), nil
		}
		lastErr = err
		var capacity *executionservices.SandboxCapacityError
		if errors.As(err, &capacity) {
			softAllowance := capacity.Slots
			if !capacity.Strict && definition.Effective.Scaling.ConcurrencyPerWorker > 0 {
				softAllowance += capacity.Slots / definition.Effective.Scaling.ConcurrencyPerWorker
			}
			if reserve && ((!capacity.Strict && capacity.Occupied < softAllowance) || (capacity.Strict && capacity.Occupied < capacity.Slots)) && fallback == nil {
				fallback = sandbox
			}
			continue
		}
	}

	sandbox, err := m.addCapacitySandboxLocked(ctx, runtime, definition)
	if err == nil {
		if reserve {
			if activated := m.activateSandbox(runtime, sandbox, sandbox.status.WorkerIDs); activated != nil {
				return activated, nil
			}
			return nil, errors.New("service version changed while activating capacity")
		}
		return sandbox, nil
	}
	lastErr = err
	if fallback != nil {
		if activated := m.activateSandbox(runtime, fallback, fallback.status.WorkerIDs); activated != nil {
			return activated, nil
		}
		return nil, errors.New("service version changed while selecting fallback capacity")
	}
	if lastErr == nil {
		lastErr = errors.New("all configured execution slots are occupied")
	}
	return nil, fmt.Errorf("%w: %v", executionservices.ErrSandboxCapacity, lastErr)
}

func (m *Manager) triggerCapacityReconcile(runtime *runtimeService, definition workspacepackages.Definition) {
	lock := m.serviceCapacityLock(definition.Identity.ServiceID())
	if !lock.TryLock() {
		return
	}
	m.mu.Lock()
	if m.stopBackground == nil {
		m.mu.Unlock()
		lock.Unlock()
		return
	}
	background := m.background
	m.wait.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wait.Done()
		defer lock.Unlock()
		ctx, cancel := context.WithTimeout(background, m.startup)
		defer cancel()
		_, _ = m.selectCapacitySandboxLocked(ctx, runtime, definition, false)
	}()
}

func (m *Manager) localWorkerMaximum(definition workspacepackages.Definition) int {
	if definition.Effective.Scaling.MaximumWorkers == 0 {
		return 0
	}
	return len(m.localIndexes(definition.Effective.Scaling.MaximumWorkers))
}

func (m *Manager) addCapacitySandbox(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeSandbox, error) {
	unlockCapacity := m.lockServiceCapacity(definition.Identity.ServiceID())
	defer unlockCapacity()
	return m.addCapacitySandboxLocked(ctx, runtime, definition)
}

func (m *Manager) addCapacitySandboxLocked(ctx context.Context, runtime *runtimeService, definition workspacepackages.Definition) (*runtimeSandbox, error) {
	m.mu.Lock()
	current := m.services[definition.Identity.ServiceID()]
	if current != runtime {
		m.mu.Unlock()
		return nil, errors.New("service version changed while adding capacity")
	}
	used := make(map[int]bool, len(current.sandboxes))
	for _, sandbox := range current.sandboxes {
		used[sandbox.status.Index] = true
	}
	if localMaximum := m.localWorkerMaximum(definition); localMaximum > 0 && workerCount(current.sandboxes) >= localMaximum {
		m.mu.Unlock()
		return nil, executionservices.ErrSandboxCapacity
	}
	index := m.nextOwnedSandboxIndex(used)
	if index < 0 {
		m.mu.Unlock()
		return nil, executionservices.ErrSandboxCapacity
	}
	m.mu.Unlock()

	sandbox, err := m.prepareSandbox(ctx, definition, index, 0, 1)
	if err != nil {
		m.mu.Lock()
		if current == m.services[definition.Identity.ServiceID()] {
			if len(current.sandboxes) == 0 {
				current.status.State = StatePendingCapacity
			} else {
				current.status.State = StateDegraded
			}
			current.status.CapacityResource = capacityResource(err)
			current.status.CapacityReason = err.Error()
			status := cloneRuntimeStatus(current)
			m.mu.Unlock()
			_ = m.writeObserved(status)
		} else {
			m.mu.Unlock()
		}
		return nil, fmt.Errorf("add service sandbox: %w", err)
	}
	m.mu.Lock()
	if current != m.services[definition.Identity.ServiceID()] {
		m.mu.Unlock()
		_, _ = m.pools.Stop(context.Background(), sandbox.status.PoolID)
		return nil, errors.New("service version changed while adding capacity")
	}
	current.sandboxes = append(current.sandboxes, sandbox)
	sort.Slice(current.sandboxes, func(i, j int) bool { return current.sandboxes[i].status.Index < current.sandboxes[j].status.Index })
	current.status.State = StateReady
	if !containsSandboxIndexes(current.sandboxes, m.minimumSandboxIndexes(definition)) {
		current.status.State = StateDegraded
	}
	current.status.CapacityResource = ""
	current.status.CapacityReason = ""
	current.status.Sandboxes = statusesOf(current.sandboxes)
	current.status.SandboxCount = len(current.sandboxes)
	current.status.WorkerCount = workerCount(current.sandboxes)
	status := cloneRuntimeStatus(current)
	m.mu.Unlock()
	m.scheduleMaintenance(definition.Identity.ServiceID())
	_ = m.writeObserved(status)
	return sandbox, nil
}

func (m *Manager) lockServiceCapacity(serviceID string) func() {
	lock := m.serviceCapacityLock(serviceID)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) serviceCapacityLock(serviceID string) *sync.Mutex {
	value, _ := m.capacityLocks.LoadOrStore(serviceID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (m *Manager) nextOwnedSandboxIndex(used map[int]bool) int {
	if m.nodes == nil {
		for candidate := 0; ; candidate++ {
			if !used[candidate] {
				return candidate
			}
		}
	}
	for limit := max(16, len(used)*2+1); limit <= 1<<20; limit *= 2 {
		for _, candidate := range m.nodes.LocalIndexes(limit) {
			if !used[candidate] {
				return candidate
			}
		}
	}
	return -1
}

func (m *Manager) activateSandbox(runtime *runtimeService, sandbox *runtimeSandbox, workerIDs []string) *runtimeSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || sandbox == nil || sandbox.status.Version != runtime.status.LoadedVersion || m.services[runtime.status.ServiceID] != runtime || !slices.Contains(runtime.sandboxes, sandbox) {
		return nil
	}
	sandbox.status.WorkerIDs = append([]string(nil), workerIDs...)
	reserveRequest(sandbox, time.Now())
	runtimeMetrics(runtime).ActiveRequests++
	if runtime.status.State == StateIdle {
		runtime.status.State = StateReady
	}
	runtime.status.Sandboxes = statusesOf(runtime.sandboxes)
	runtime.status.SandboxCount = len(runtime.sandboxes)
	runtime.status.WorkerCount = workerCount(runtime.sandboxes)
	return sandbox
}

func (m *Manager) updateSandboxWorkers(runtime *runtimeService, sandbox *runtimeSandbox, workerIDs []string) *runtimeSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || sandbox == nil || sandbox.status.Version != runtime.status.LoadedVersion || m.services[runtime.status.ServiceID] != runtime || !slices.Contains(runtime.sandboxes, sandbox) {
		return nil
	}
	sandbox.status.WorkerIDs = append([]string(nil), workerIDs...)
	runtime.status.Sandboxes = statusesOf(runtime.sandboxes)
	runtime.status.SandboxCount = len(runtime.sandboxes)
	runtime.status.WorkerCount = workerCount(runtime.sandboxes)
	return sandbox
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

func persistentSandbox(value *persistentDispatch) *runtimeSandbox {
	if value == nil {
		return nil
	}
	return value.sandbox
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
		sandbox := m.selectPersistentSandbox(runtime, record.PoolID, record.WorkerID)
		if sandbox == nil {
			m.persistentRoutes.discard(token)
			return nil, errRouteNotFound
		}
		return &persistentDispatch{token: token, record: record, sandbox: sandbox}, nil
	}
	sandbox, err := m.selectCapacitySandbox(ctx, runtime, definition)
	if err != nil {
		return nil, err
	}
	token, record, err := m.persistentRoutes.create(serviceID, sandbox.status.PoolID, sandbox.status.RuntimeGroupID, sandbox.status.SandboxID, userID, definition.Effective.Lifecycle.SessionKeepAlive, connect)
	if err != nil {
		m.finishRequest(runtime, sandbox, 0, 0, 0, false)
		return nil, err
	}
	return &persistentDispatch{token: token, record: record, sandbox: sandbox, initial: true}, nil
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

func (m *Manager) selectPersistentSandbox(runtime *runtimeService, poolID, workerID string) *runtimeSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil {
		return nil
	}
	var selected *runtimeSandbox
	for _, sandbox := range runtime.sandboxes {
		if sandbox.status.PoolID == poolID {
			selected = sandbox
			break
		}
	}
	if selected == nil || selected.status.Version != runtime.status.LoadedVersion || workerID != "" && !slices.Contains(selected.status.WorkerIDs, workerID) {
		return nil
	}
	reserveRequest(selected, time.Now())
	runtimeMetrics(runtime).ActiveRequests++
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

func (m *Manager) finishRequest(runtime *runtimeService, sandbox *runtimeSandbox, status int, bytes uint64, duration time.Duration, timeout bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil {
		return
	}
	releaseRequest(sandbox, time.Now())
	metrics := runtimeMetrics(runtime)
	metrics.ActiveRequests = max(metrics.ActiveRequests-1, 0)
	metrics.RequestCount++
	metrics.RequestDuration = duration
	metrics.BytesStreamed += bytes
	if timeout {
		metrics.TimeoutCount++
	}
	if status != 0 {
		metrics.ResponseStatus[strconv.Itoa(status)]++
	}
}

func activeReservations(sandbox *runtimeSandbox, now time.Time) int {
	if sandbox == nil {
		return 0
	}
	kept := sandbox.reservations[:0]
	for _, expiresAt := range sandbox.reservations {
		if now.Before(expiresAt) {
			kept = append(kept, expiresAt)
		}
	}
	sandbox.reservations = kept
	return len(kept)
}

func reserveRequest(sandbox *runtimeSandbox, now time.Time) {
	activeReservations(sandbox, now)
	sandbox.reservations = append(sandbox.reservations, now.Add(dispatchReservationLifetime))
}

func releaseRequest(sandbox *runtimeSandbox, now time.Time) {
	activeReservations(sandbox, now)
	if count := len(sandbox.reservations); count > 0 {
		sandbox.reservations = sandbox.reservations[:count-1]
	}
}

func (m *Manager) statusFromDefinition(definition workspacepackages.Definition, state State) Status {
	return Status{ServiceID: definition.Identity.ServiceID(), PackageID: definition.Identity.PackageID(), CanonicalBasePath: definition.Identity.CanonicalBasePath(), Description: definition.Service.Description, ServiceType: definition.Effective.Lifecycle.ServiceType, AccessMode: definition.Service.Access.Mode, Enabled: definition.State.Enabled, DesiredVersion: definition.State.Generation, State: state, Entrypoint: definition.EntrypointPath, Effective: definition.Effective, Metrics: emptyMetrics()}
}

func isSessionService(definition workspacepackages.Definition) bool {
	return definition.Effective.Lifecycle.ServiceType == workspacepackages.ServiceTypeSession
}

func serviceExecutionMode(definition workspacepackages.Definition) string {
	if isSessionService(definition) {
		return "persistent"
	}
	return "stateless"
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
func versionPoolID(serviceID string, version uint64, index int) string {
	return hashedID("service", fmt.Sprintf("%s\x00%d\x00%d", serviceID, version, index))
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
func sandboxesOf(service *runtimeService) []*runtimeSandbox {
	if service == nil {
		return nil
	}
	return service.sandboxes
}

func observedSandboxesOf(service *runtimeService) []*runtimeSandbox {
	if service == nil {
		return nil
	}
	result := make([]*runtimeSandbox, 0, len(service.sandboxes)+len(service.retired))
	result = append(result, service.sandboxes...)
	result = append(result, service.retired...)
	return result
}

func runtimeMetrics(service *runtimeService) *Metrics {
	if service != nil && service.metrics != nil {
		return service.metrics
	}
	metrics := emptyMetrics()
	if service != nil {
		metrics = cloneMetrics(service.status.Metrics)
		service.metrics = &metrics
	}
	return &metrics
}

func replacementRuntime(
	existing *runtimeService,
	status Status,
	sandboxes []*runtimeSandbox,
	retired []*runtimeSandbox,
	definition workspacepackages.Definition,
) *runtimeService {
	metrics := runtimeMetrics(existing)
	status.Metrics = cloneMetrics(*metrics)
	return &runtimeService{
		status: status, sandboxes: sandboxes, retired: retired,
		metrics: metrics, definition: definition,
	}
}

func cloneRuntimeStatus(service *runtimeService) Status {
	if service == nil {
		return Status{}
	}
	status := cloneStatus(service.status)
	status.Metrics = cloneMetrics(*runtimeMetrics(service))
	return status
}

func (m *Manager) observedStatusLocked(status Status, current []*runtimeSandbox) Status {
	status.Sandboxes, status.WorkerCount, status.VersionCount = observedSandboxes(current, status)
	status.SandboxCount = len(status.Sandboxes)
	if status.WorkerCount > 0 {
		switch status.State {
		case StateIdle:
			status.State = StateReady
		case StatePendingCapacity, StateFailed:
			status.State = StateDegraded
		}
	}
	return status
}
func observedSandboxes(sandboxes []*runtimeSandbox, service Status) ([]ServiceSandboxStatus, int, int) {
	result := make([]ServiceSandboxStatus, 0, len(sandboxes))
	bySandbox := make(map[string]int, len(sandboxes))
	workers, versions := map[string]bool{}, map[uint64]bool{}
	for _, sandbox := range sandboxes {
		status := sandbox.status
		versions[status.Version] = true
		for _, workerID := range status.WorkerIDs {
			if workerID != "" {
				workers[workerID] = true
			}
		}
		status.WorkerIDs = uniqueStrings(status.WorkerIDs)
		key := status.SandboxID
		if key == "" {
			key = "\x00" + status.PoolID
		}
		if index, exists := bySandbox[key]; exists {
			merged := &result[index]
			merged.WorkerIDs = uniqueStrings(append(merged.WorkerIDs, status.WorkerIDs...))
			merged.ActiveRequests = max(merged.ActiveRequests, status.ActiveRequests)
			merged.ActiveExecutions = max(merged.ActiveExecutions, status.ActiveExecutions)
			if status.Version > merged.Version {
				merged.Index, merged.Version, merged.PoolID = status.Index, status.Version, status.PoolID
			}
			continue
		}
		bySandbox[key] = len(result)
		result = append(result, status)
	}
	if service.VersionCount > 0 {
		versions[service.LoadedVersion] = true
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Version != result[j].Version {
			return result[i].Version > result[j].Version
		}
		if result[i].Index != result[j].Index {
			return result[i].Index < result[j].Index
		}
		return result[i].SandboxID < result[j].SandboxID
	})
	return result, len(workers), len(versions)
}
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
func statusesOf(sandboxes []*runtimeSandbox) []ServiceSandboxStatus {
	result := make([]ServiceSandboxStatus, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		status := sandbox.status
		status.WorkerIDs = append([]string{}, status.WorkerIDs...)
		result = append(result, status)
	}
	return result
}
func workerCount(sandboxes []*runtimeSandbox) int {
	total := 0
	for _, sandbox := range sandboxes {
		total += len(sandbox.status.WorkerIDs)
	}
	return total
}
func emptyMetrics() Metrics { return Metrics{ResponseStatus: map[string]uint64{}} }
func cloneMetrics(value Metrics) Metrics {
	value.ResponseStatus = cloneMap(value.ResponseStatus)
	return value
}
func cloneStatus(value Status) Status {
	value.Sandboxes = append([]ServiceSandboxStatus(nil), value.Sandboxes...)
	for index := range value.Sandboxes {
		value.Sandboxes[index].WorkerIDs = append([]string{}, value.Sandboxes[index].WorkerIDs...)
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
