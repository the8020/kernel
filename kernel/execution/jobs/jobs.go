// Package jobs owns durable job execution and optional compatible Worker reuse.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
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
}
type WorkerManager interface {
	Start(context.Context, string, supervisor.StartWorkerRequest) (workers.Record, error)
	RunJob(context.Context, string, any, []string) (supervisor.JobResult, error)
	Stop(context.Context, string, bool) error
}
type Policy struct {
	Strategy             model.GroupingStrategy
	Profile              model.RuntimeProfile
	Resources            model.ResourceLimits
	Lifecycle            model.LifecyclePolicy
	MaximumParallel      int
	QueuedExecutionLimit int
	ExecutionTimeout     time.Duration
	Reuse                bool
	IdleRuntimeTimeout   time.Duration
	WorkspaceMounts      executionprofile.MountPolicy
	Logger               *slog.Logger
}
type Options struct {
	OwnerID           string
	Input             any
	Detached          bool
	GroupKey          string
	Namespace         string
	Timeout           time.Duration
	Parallelism       int
	Reuse             *bool
	Permissions       *supervisor.WorkerPermissions
	ReleaseID         string
	Workspace         string
	WorkspaceWritable bool
	DatabaseAccess    string
	CheckModules      []string
	Mounts            []model.Mount
}
type Record struct {
	ExecutionID        string                       `json:"execution_id"`
	JobID              string                       `json:"job_id"`
	OwnerID            string                       `json:"owner_id"`
	ProfileHash        string                       `json:"profile_hash"`
	Entrypoint         string                       `json:"entrypoint"`
	RuntimeGroupID     string                       `json:"runtime_group_id,omitempty"`
	SandboxID          string                       `json:"sandbox_id,omitempty"`
	WorkerID           string                       `json:"worker_id"`
	ReleaseID          string                       `json:"release_id"`
	State              string                       `json:"state"`
	Detached           bool                         `json:"detached"`
	Reuse              bool                         `json:"reuse"`
	Input              any                          `json:"input,omitempty"`
	Result             any                          `json:"result,omitempty"`
	Logs               []supervisor.LogEvent        `json:"logs,omitempty"`
	Failure            string                       `json:"failure,omitempty"`
	QueuedAt           time.Time                    `json:"queued_at,omitempty"`
	StartedAt          time.Time                    `json:"started_at"`
	FinishedAt         time.Time                    `json:"finished_at,omitempty"`
	Timeout            time.Duration                `json:"timeout"`
	Parallelism        int                          `json:"parallelism"`
	Duration           time.Duration                `json:"duration"`
	Permissions        supervisor.WorkerPermissions `json:"permissions"`
	DatabaseAccess     string                       `json:"database_access,omitempty"`
	CheckModules       []string                     `json:"check_modules,omitempty"`
	ModuleDependencies map[string][]string          `json:"module_dependencies,omitempty"`
}
type Manager struct {
	mu          sync.Mutex
	coordinator GroupCoordinator
	workers     WorkerManager
	store       *records.Store
	policy      Policy
	now         func() time.Time
	timers      map[string]*time.Timer
	lifecycle   context.Context
	cancel      context.CancelFunc
	wait        sync.WaitGroup
	closed      bool
	lastQueued  time.Time
	logger      *slog.Logger
}

func New(groupCoordinator GroupCoordinator, workerManager WorkerManager, store *records.Store, policy Policy) (*Manager, error) {
	if groupCoordinator == nil || workerManager == nil || store == nil {
		return nil, errors.New("group coordinator, Worker manager, and job store are required")
	}
	if policy.Strategy == "" {
		policy.Strategy = model.GroupingOwner
	}
	if !policy.Strategy.Valid() {
		return nil, errors.New("valid job grouping strategy is required")
	}
	if policy.MaximumParallel <= 0 {
		policy.MaximumParallel = 4
	}
	if policy.QueuedExecutionLimit <= 0 {
		policy.QueuedExecutionLimit = 1024
	}
	if policy.ExecutionTimeout <= 0 {
		policy.ExecutionTimeout = 5 * time.Minute
	}
	if policy.IdleRuntimeTimeout <= 0 {
		policy.IdleRuntimeTimeout = time.Minute
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	return &Manager{coordinator: groupCoordinator, workers: workerManager, store: store, policy: policy, now: func() time.Time { return time.Now().UTC() }, timers: map[string]*time.Timer{}, lifecycle: lifecycle, cancel: cancel, logger: policy.Logger}, nil
}

type submission struct {
	record    Record
	profile   model.RuntimeProfile
	groupKey  string
	namespace string
}

func (m *Manager) Run(ctx context.Context, jobID, entrypoint string, options Options) (Record, error) {
	if jobID == "" || entrypoint == "" {
		return Record{}, errors.New("job ID and entrypoint are required")
	}
	ownerID := options.OwnerID
	if ownerID == "" {
		ownerID = jobID
	}
	profile, err := executionprofile.ForWorkerWithWorkspace(m.policy.Profile, options.Permissions, executionprofile.Workspace{Source: options.Workspace, OwnerID: ownerID, Writable: options.WorkspaceWritable}, m.policy.WorkspaceMounts)
	if err != nil {
		return Record{}, err
	}
	for _, requested := range options.Mounts {
		if m.policy.WorkspaceMounts == nil {
			return Record{}, errors.New("job mounts are unavailable")
		}
		mount, err := m.policy.WorkspaceMounts.Validate(requested)
		if err != nil {
			return Record{}, fmt.Errorf("job mount: %w", err)
		}
		if !mount.ReadOnly {
			return Record{}, errors.New("additional job mounts must be read-only")
		}
		profile.Mounts = append(profile.Mounts, mount)
		profile.Permissions.ReadPaths = append(profile.Permissions.ReadPaths, mount.Target)
	}
	profileHash, err := profile.Hash()
	if err != nil {
		return Record{}, err
	}
	permissions := permissionsFor(profile.Permissions)
	if options.Permissions != nil {
		permissions = *options.Permissions
	}
	if options.ReleaseID == "" {
		options.ReleaseID = "development"
	}
	limit := m.policy.MaximumParallel
	if options.Parallelism > 0 && options.Parallelism < limit {
		limit = options.Parallelism
	}
	reuse := m.policy.Reuse
	if options.Reuse != nil {
		reuse = *options.Reuse
	}
	executionID, err := model.NewID("execution")
	if err != nil {
		return Record{}, err
	}
	workerID, err := model.NewWorkerID()
	if err != nil {
		return Record{}, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = m.policy.ExecutionTimeout
	}
	databaseAccess := options.DatabaseAccess
	if databaseAccess == "" {
		databaseAccess = "full"
	}
	if databaseAccess != "full" && databaseAccess != "metadata" && databaseAccess != "none" {
		return Record{}, errors.New("job database access must be full, metadata, or none")
	}
	record := Record{ExecutionID: executionID, JobID: jobID, OwnerID: ownerID, ProfileHash: profileHash, Entrypoint: entrypoint, WorkerID: workerID, ReleaseID: options.ReleaseID, State: "STARTING", Detached: options.Detached, Reuse: reuse, Input: options.Input, Timeout: timeout, Parallelism: limit, Permissions: permissions, DatabaseAccess: databaseAccess, CheckModules: append([]string(nil), options.CheckModules...)}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, errors.New("job manager is closed")
	}
	record.QueuedAt = m.now()
	if !record.QueuedAt.After(m.lastQueued) {
		record.QueuedAt = m.lastQueued.Add(time.Nanosecond)
	}
	m.lastQueued = record.QueuedAt
	record.StartedAt = record.QueuedAt
	active, err := m.activeCount()
	if err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	if active >= limit {
		queued, countErr := m.queuedCount()
		if countErr != nil {
			m.mu.Unlock()
			return Record{}, countErr
		}
		if queued >= m.policy.QueuedExecutionLimit {
			m.mu.Unlock()
			return Record{}, fmt.Errorf("job queue limit %d reached", m.policy.QueuedExecutionLimit)
		}
		record.State = "QUEUED"
		record.StartedAt = time.Time{}
	}
	if err := m.store.Save(executionID, record); err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	m.mu.Unlock()
	prepared := submission{record: record, profile: profile, groupKey: options.GroupKey, namespace: options.Namespace}
	if record.State == "QUEUED" {
		if record.Detached {
			if err := m.launch(func(background context.Context) {
				bounded, cancel := context.WithTimeout(background, record.Timeout)
				defer cancel()
				_, _ = m.awaitQueued(bounded, prepared)
			}); err != nil {
				return m.failQueued(record, err)
			}
			return record, nil
		}
		bounded, cancel := context.WithTimeout(ctx, record.Timeout)
		defer cancel()
		return m.awaitQueued(bounded, prepared)
	}
	bounded, cancel := context.WithTimeout(ctx, record.Timeout)
	defer cancel()
	return m.start(bounded, prepared)
}

func (m *Manager) start(ctx context.Context, prepared submission) (Record, error) {
	record := prepared.record
	m.mu.Lock()
	current, err := m.Inspect(record.ExecutionID)
	if err != nil {
		m.mu.Unlock()
		return record, err
	}
	if current.State != "STARTING" {
		m.mu.Unlock()
		return current, fmt.Errorf("job execution entered %s before startup", current.State)
	}
	record = current
	if record.Reuse {
		if reusable, ok := m.reusable(record.JobID, record.Entrypoint, record.OwnerID, record.ReleaseID, record.ProfileHash, record.DatabaseAccess, record.Permissions); ok {
			m.stopIdleTimerLocked(reusable.ExecutionID)
			available := reusable
			available.State = "SUCCEEDED"
			available.Reuse = false
			if err := m.store.Save(available.ExecutionID, available); err != nil {
				m.mu.Unlock()
				return record, err
			}
			record.RuntimeGroupID, record.SandboxID, record.WorkerID = reusable.RuntimeGroupID, reusable.SandboxID, reusable.WorkerID
			record.State = "RUNNING"
			if err := m.store.Save(record.ExecutionID, record); err != nil {
				_ = m.store.Save(reusable.ExecutionID, reusable)
				m.mu.Unlock()
				return record, err
			}
			m.mu.Unlock()
			return m.execute(ctx, record)
		}
	}
	m.mu.Unlock()
	group, err := m.coordinator.Ensure(ctx, coordinator.Request{WorkloadType: model.WorkloadJob, OwnerID: record.OwnerID, ExecutionID: record.ExecutionID, Namespace: prepared.namespace, ExplicitGroupKey: prepared.groupKey, Strategy: m.policy.Strategy, Profile: prepared.profile, ResourceLimits: m.policy.Resources, Lifecycle: m.policy.Lifecycle})
	if err != nil {
		return m.fail(record, err)
	}
	record.RuntimeGroupID, record.SandboxID = group.Spec.RuntimeGroupID, group.Spec.SandboxID
	if current, proceed := m.starting(record.ExecutionID); !proceed {
		return current, fmt.Errorf("job execution entered %s before Worker startup", current.State)
	}
	started, err := m.workers.Start(ctx, group.Spec.RuntimeGroupID, supervisor.StartWorkerRequest{Metadata: supervisor.ExecutionMetadata{WorkerID: record.WorkerID, ExecutionID: record.ExecutionID, WorkloadType: model.WorkloadJob, OwnerID: record.OwnerID, WorkloadID: record.JobID, ReleaseID: record.ReleaseID, Entrypoint: record.Entrypoint, DebuggerName: "job:" + record.OwnerID + ":" + record.ExecutionID + ":" + record.WorkerID, DatabaseAccess: record.DatabaseAccess}, Permissions: record.Permissions})
	if err != nil {
		return m.fail(record, err)
	}
	record.WorkerID = started.Worker.WorkerID
	m.mu.Lock()
	current, inspectErr := m.Inspect(record.ExecutionID)
	if inspectErr != nil || current.State != "STARTING" {
		m.mu.Unlock()
		_ = m.workers.Stop(context.Background(), record.WorkerID, true)
		if inspectErr != nil {
			return record, inspectErr
		}
		return current, fmt.Errorf("job execution entered %s during Worker startup", current.State)
	}
	record.State = "RUNNING"
	if err := m.store.Save(record.ExecutionID, record); err != nil {
		m.mu.Unlock()
		_ = m.workers.Stop(context.Background(), record.WorkerID, true)
		return record, err
	}
	m.mu.Unlock()
	return m.execute(ctx, record)
}

func (m *Manager) awaitQueued(ctx context.Context, prepared submission) (Record, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		current, err := m.Inspect(prepared.record.ExecutionID)
		if err != nil {
			m.mu.Unlock()
			return prepared.record, err
		}
		if current.State != "QUEUED" {
			m.mu.Unlock()
			if current.State == "CANCELLED" {
				return current, context.Canceled
			}
			return current, nil
		}
		items, err := m.List()
		if err != nil {
			m.mu.Unlock()
			return current, err
		}
		active := 0
		head := current.ExecutionID
		for _, item := range items {
			if item.State == "STARTING" || item.State == "RUNNING" {
				active++
			}
			if item.State == "QUEUED" && queueLess(item, current) {
				head = item.ExecutionID
			}
		}
		if head == current.ExecutionID && active < current.Parallelism {
			current.State = "STARTING"
			current.StartedAt = m.now()
			if current.StartedAt.Before(current.QueuedAt) {
				current.StartedAt = current.QueuedAt
			}
			if err := m.store.Save(current.ExecutionID, current); err != nil {
				m.mu.Unlock()
				return current, err
			}
			m.mu.Unlock()
			prepared.record = current
			return m.start(ctx, prepared)
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			_ = m.Cancel(context.Background(), current.ExecutionID)
			cancelled, _ := m.Inspect(current.ExecutionID)
			return cancelled, ctx.Err()
		case <-ticker.C:
		}
	}
}

func queueLess(left, right Record) bool {
	if left.QueuedAt.Equal(right.QueuedAt) {
		return left.ExecutionID < right.ExecutionID
	}
	return left.QueuedAt.Before(right.QueuedAt)
}

func (m *Manager) starting(executionID string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.Inspect(executionID)
	return record, err == nil && record.State == "STARTING"
}

func (m *Manager) execute(parent context.Context, record Record) (Record, error) {
	run := func(ctx context.Context, running Record) (Record, error) {
		result, err := m.workers.RunJob(ctx, running.WorkerID, running.Input, running.CheckModules)
		if err != nil {
			return m.failAndStop(running, err)
		}
		running.Result = result.Result
		running.Logs = append([]supervisor.LogEvent(nil), result.Logs...)
		running.ModuleDependencies = cloneDependencies(result.ModuleDependencies)
		running.FinishedAt = m.now()
		running.Duration = running.FinishedAt.Sub(running.StartedAt)
		if running.Reuse {
			running.State = "IDLE"
		} else {
			running.State = "SUCCEEDED"
			if stopErr := m.workers.Stop(context.Background(), running.WorkerID, false); stopErr != nil {
				running.Failure = stopErr.Error()
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		current, inspectErr := m.Inspect(running.ExecutionID)
		if inspectErr == nil && current.State != "RUNNING" {
			return current, nil
		}
		if err := m.store.Save(running.ExecutionID, running); err != nil {
			return Record{}, err
		}
		if running.State == "IDLE" {
			m.scheduleIdleTimerLocked(running)
		}
		return running, nil
	}
	if record.Detached {
		backgroundRecord := record
		if err := m.launch(func(background context.Context) {
			ctx, cancel := context.WithTimeout(background, backgroundRecord.Timeout)
			defer cancel()
			_, _ = run(ctx, backgroundRecord)
		}); err != nil {
			return m.failAndStop(record, err)
		}
		return record, nil
	}
	ctx, cancel := context.WithTimeout(parent, record.Timeout)
	defer cancel()
	return run(ctx, record)
}

func cloneDependencies(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for module, dependencies := range source {
		result[module] = append([]string(nil), dependencies...)
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
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].StartedAt, result[j].StartedAt
		if !result[i].QueuedAt.IsZero() {
			left = result[i].QueuedAt
		}
		if !result[j].QueuedAt.IsZero() {
			right = result[j].QueuedAt
		}
		if left.Equal(right) {
			return result[i].ExecutionID < result[j].ExecutionID
		}
		return left.Before(right)
	})
	return result, nil
}

func (m *Manager) recoverableRecord(executionID string) (Record, bool) {
	record, err := m.Inspect(executionID)
	if err == nil && record.ExecutionID == executionID {
		return record, true
	}
	if err == nil {
		err = fmt.Errorf("job execution %q: persisted identity is %q", executionID, record.ExecutionID)
	}
	path, quarantineErr := m.store.Quarantine(executionID)
	if m.logger != nil {
		if quarantineErr != nil {
			m.logger.Error("skip invalid job record; quarantine failed", "execution_id", executionID, "error", err, "quarantine_error", quarantineErr)
		} else {
			m.logger.Error("quarantined invalid job record", "execution_id", executionID, "path", path, "error", err)
		}
	}
	return Record{}, false
}

func (m *Manager) Inspect(executionID string) (Record, error) {
	var record Record
	if err := m.store.Load(executionID, &record); err != nil {
		return record, fmt.Errorf("job execution %q: %w", executionID, err)
	}
	return record, nil
}
func (m *Manager) Cancel(ctx context.Context, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.Inspect(executionID)
	if err != nil {
		return err
	}
	if record.State != "QUEUED" && record.State != "STARTING" && record.State != "RUNNING" {
		return nil
	}
	m.stopIdleTimerLocked(executionID)
	if record.State == "RUNNING" {
		if err := m.workers.Stop(ctx, record.WorkerID, true); err != nil {
			return err
		}
	}
	record.State = "CANCELLED"
	record.FinishedAt = m.now()
	return m.store.Save(executionID, record)
}

// FailGroup fails active executions and removes lost reusable capacity when a
// job runtime group terminates.
func (m *Manager) FailGroup(runtimeGroupID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.List()
	if err != nil {
		return err
	}
	var joined error
	for _, record := range items {
		if record.RuntimeGroupID != runtimeGroupID {
			continue
		}
		switch record.State {
		case "STARTING", "RUNNING":
			record.State = "FAILED"
			record.Failure = reason
			record.FinishedAt = m.now()
			record.Duration = record.FinishedAt.Sub(record.StartedAt)
		case "IDLE":
			m.stopIdleTimerLocked(record.ExecutionID)
			record.State = "SUCCEEDED"
			record.Reuse = false
		default:
			continue
		}
		joined = errors.Join(joined, m.store.Save(record.ExecutionID, record))
	}
	return joined
}

// Restore resolves transient executions without replay and restores bounded
// retirement timers for compatible idle Workers after a kernel restart.
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.List()
	if err != nil {
		return err
	}
	var joined error
	for _, record := range items {
		switch record.State {
		case "QUEUED":
			record.State = "FAILED"
			record.Failure = "kernel restarted while job execution was queued; execution was not replayed"
			record.FinishedAt = m.now()
			joined = errors.Join(joined, m.store.Save(record.ExecutionID, record))
		case "STARTING", "RUNNING":
			stopErr := error(nil)
			if record.WorkerID != "" {
				stopErr = m.workers.Stop(ctx, record.WorkerID, true)
			}
			record.State = "FAILED"
			record.Failure = "kernel restarted while job execution was active; execution was not replayed"
			if stopErr != nil {
				record.Failure += ": " + stopErr.Error()
			}
			record.FinishedAt = m.now()
			record.Duration = record.FinishedAt.Sub(record.StartedAt)
			joined = errors.Join(joined, stopErr, m.store.Save(record.ExecutionID, record))
		case "IDLE":
			m.scheduleIdleTimerLocked(record)
		}
	}
	return joined
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wait.Wait()
		return nil
	}
	m.closed = true
	m.cancel()
	for id := range m.timers {
		m.stopIdleTimerLocked(id)
	}
	m.mu.Unlock()
	m.wait.Wait()
	return nil
}

func (m *Manager) scheduleIdleTimerLocked(record Record) {
	m.stopIdleTimerLocked(record.ExecutionID)
	delay := m.policy.IdleRuntimeTimeout
	if !record.FinishedAt.IsZero() {
		delay -= m.now().Sub(record.FinishedAt)
	}
	if delay < 0 {
		delay = 0
	}
	m.timers[record.ExecutionID] = time.AfterFunc(delay, func() { m.retireIdle(record.ExecutionID) })
}

func (m *Manager) stopIdleTimerLocked(executionID string) {
	if timer := m.timers[executionID]; timer != nil {
		timer.Stop()
		delete(m.timers, executionID)
	}
}

func (m *Manager) retireIdle(executionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.timers, executionID)
	record, err := m.Inspect(executionID)
	if err != nil || record.State != "IDLE" || !record.Reuse {
		return
	}
	if err := m.workers.Stop(context.Background(), record.WorkerID, false); err != nil {
		record.Failure = "retire idle reusable Worker: " + err.Error()
	}
	record.State = "SUCCEEDED"
	record.Reuse = false
	_ = m.store.Save(record.ExecutionID, record)
}
func (m *Manager) activeCount() (int, error) {
	items, err := m.List()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		if item.State == "STARTING" || item.State == "RUNNING" {
			count++
		}
	}
	return count, nil
}

func (m *Manager) queuedCount() (int, error) {
	items, err := m.List()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		if item.State == "QUEUED" {
			count++
		}
	}
	return count, nil
}

func (m *Manager) launch(run func(context.Context)) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("job manager is closed")
	}
	ctx := m.lifecycle
	m.wait.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wait.Done()
		run(ctx)
	}()
	return nil
}
func (m *Manager) reusable(jobID, entrypoint, ownerID, releaseID, profileHash, databaseAccess string, permissions supervisor.WorkerPermissions) (Record, bool) {
	items, err := m.List()
	if err != nil {
		return Record{}, false
	}
	for _, item := range items {
		if item.JobID == jobID && item.OwnerID == ownerID && item.ReleaseID == releaseID && item.ProfileHash == profileHash && item.DatabaseAccess == databaseAccess && reflect.DeepEqual(item.Permissions, permissions) && item.Entrypoint == entrypoint && item.State == "IDLE" && item.Reuse {
			return item, true
		}
	}
	return Record{}, false
}
func (m *Manager) fail(record Record, cause error) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.Inspect(record.ExecutionID)
	if err == nil && current.State != "STARTING" && current.State != "RUNNING" {
		return current, cause
	}
	record.State = "FAILED"
	record.Failure = cause.Error()
	record.FinishedAt = m.now()
	record.Duration = record.FinishedAt.Sub(record.StartedAt)
	_ = m.store.Save(record.ExecutionID, record)
	return record, cause
}

func (m *Manager) failQueued(record Record, cause error) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.Inspect(record.ExecutionID)
	if err == nil {
		record = current
	}
	if record.State == "QUEUED" {
		record.State = "FAILED"
		record.Failure = cause.Error()
		record.FinishedAt = m.now()
		_ = m.store.Save(record.ExecutionID, record)
	}
	return record, cause
}
func (m *Manager) failAndStop(record Record, cause error) (Record, error) {
	if record.WorkerID != "" {
		_ = m.workers.Stop(context.Background(), record.WorkerID, true)
	}
	return m.fail(record, cause)
}
func permissionsFor(value model.Permissions) supervisor.WorkerPermissions {
	sys := []string(nil)
	if value.SystemInfo {
		sys = []string{"hostname", "osRelease"}
	}
	return supervisor.WorkerPermissions{Read: append([]string(nil), value.ReadPaths...), Write: append([]string(nil), value.WritePaths...), Net: append([]string(nil), value.NetworkHosts...), Import: append([]string(nil), value.ImportHosts...), Env: append([]string(nil), value.Environment...), Sys: sys}
}
