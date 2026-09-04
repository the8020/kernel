// Package jobs owns non-durable job execution and optional compatible Worker
// reuse. It keeps only live process state; scheduling and execution history
// belong to higher-level packages.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/coordinator"
	executionprofile "the8020/kernel/execution/profile"
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
	RunJob(context.Context, string, []any, map[string]string, []string) (supervisor.JobResult, error)
	StopInGroup(context.Context, string, string, bool) error
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
	Arguments         []any
	Secrets           map[string]string
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

// Record is an in-process view of one job. Result and Logs are returned to the
// invoking caller but are never written to disk or retained after a one-time
// invocation ends.
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
	Result             any                          `json:"result,omitempty"`
	Logs               []supervisor.LogEvent        `json:"logs,omitempty"`
	Failure            string                       `json:"failure,omitempty"`
	QueuedAt           time.Time                    `json:"queued_at,omitempty"`
	StartedAt          time.Time                    `json:"started_at"`
	FinishedAt         time.Time                    `json:"finished_at,omitempty"`
	Timeout            time.Duration                `json:"timeout"`
	Parallelism        int                          `json:"parallelism"`
	CallerExecutionID  string                       `json:"-"`
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
	policy      Policy
	now         func() time.Time
	records     map[string]Record
	timers      map[string]*time.Timer
	lifecycle   context.Context
	cancel      context.CancelFunc
	wait        sync.WaitGroup
	closed      bool
	lastQueued  time.Time
	queueChange chan struct{}
	logger      *slog.Logger
}

func New(groupCoordinator GroupCoordinator, workerManager WorkerManager, policy Policy) (*Manager, error) {
	if groupCoordinator == nil || workerManager == nil {
		return nil, errors.New("group coordinator and Worker manager are required")
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
	return &Manager{
		coordinator: groupCoordinator, workers: workerManager, policy: policy,
		now: func() time.Time { return time.Now().UTC() }, records: map[string]Record{},
		timers: map[string]*time.Timer{}, lifecycle: lifecycle, cancel: cancel,
		queueChange: make(chan struct{}), logger: policy.Logger,
	}, nil
}

type submission struct {
	record    Record
	profile   model.RuntimeProfile
	groupKey  string
	namespace string
	arguments []any
	secrets   map[string]string
}

func (m *Manager) Run(ctx context.Context, jobID, entrypoint string, options Options) (Record, error) {
	prepared, err := m.prepare(jobID, entrypoint, options)
	if err != nil {
		return Record{}, err
	}
	if caller, ok := execution.CallerFromContext(ctx); ok && caller.Workload == model.WorkloadJob {
		prepared.record.CallerExecutionID = caller.ExecutionID
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		clearSecrets(prepared.secrets)
		return Record{}, errors.New("job manager is closed")
	}
	prepared.record.QueuedAt = m.nextQueuedTimeLocked()
	prepared.record.StartedAt = prepared.record.QueuedAt
	if !m.canStartLocked(prepared.record) {
		if m.queuedCountLocked() >= m.policy.QueuedExecutionLimit {
			m.mu.Unlock()
			clearSecrets(prepared.secrets)
			return Record{}, fmt.Errorf("job queue limit %d reached", m.policy.QueuedExecutionLimit)
		}
		prepared.record.State = "QUEUED"
		prepared.record.StartedAt = time.Time{}
	}
	m.records[prepared.record.ExecutionID] = prepared.record
	m.signalQueueLocked()
	m.mu.Unlock()

	if prepared.record.Detached {
		initial := prepared.record
		if err := m.launch(func(background context.Context) {
			defer clearSecrets(prepared.secrets)
			bounded, cancel := context.WithTimeout(background, prepared.record.Timeout)
			defer cancel()
			_, _ = m.runPrepared(bounded, prepared)
			m.removeTerminal(prepared.record.ExecutionID)
		}); err != nil {
			clearSecrets(prepared.secrets)
			return m.fail(prepared.record, err)
		}
		return initial, nil
	}
	defer clearSecrets(prepared.secrets)
	record, runErr := m.runPrepared(ctx, prepared)
	m.removeTerminal(prepared.record.ExecutionID)
	return record, runErr
}

func (m *Manager) prepare(jobID, entrypoint string, options Options) (submission, error) {
	if jobID == "" || entrypoint == "" {
		return submission{}, errors.New("job ID and entrypoint are required")
	}
	ownerID := options.OwnerID
	if ownerID == "" {
		ownerID = jobID
	}
	profile, err := executionprofile.ForWorkerWithWorkspace(m.policy.Profile, options.Permissions, executionprofile.Workspace{Source: options.Workspace, OwnerID: ownerID, Writable: options.WorkspaceWritable}, m.policy.WorkspaceMounts)
	if err != nil {
		return submission{}, err
	}
	for _, requested := range options.Mounts {
		if m.policy.WorkspaceMounts == nil {
			return submission{}, errors.New("job mounts are unavailable")
		}
		mount, err := m.policy.WorkspaceMounts.Validate(requested)
		if err != nil {
			return submission{}, fmt.Errorf("job mount: %w", err)
		}
		if !mount.ReadOnly {
			return submission{}, errors.New("additional job mounts must be read-only")
		}
		profile.Mounts = append(profile.Mounts, mount)
		profile.Permissions.ReadPaths = append(profile.Permissions.ReadPaths, mount.Target)
	}
	profileHash, err := profile.Hash()
	if err != nil {
		return submission{}, err
	}
	permissions := permissionsFor(profile.Permissions)
	if options.Permissions != nil {
		permissions = *options.Permissions
	}
	if options.ReleaseID == "" {
		options.ReleaseID = "development"
	}
	if options.Parallelism < 0 {
		return submission{}, errors.New("job parallelism must not be negative")
	}
	parallelism := options.Parallelism
	if parallelism == 0 || parallelism > m.policy.MaximumParallel {
		parallelism = m.policy.MaximumParallel
	}
	reuse := m.policy.Reuse
	if options.Reuse != nil {
		reuse = *options.Reuse
	}
	executionID, err := model.NewID("execution")
	if err != nil {
		return submission{}, err
	}
	workerID, err := model.NewWorkerID()
	if err != nil {
		return submission{}, err
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
		return submission{}, errors.New("job database access must be full, metadata, or none")
	}
	arguments := append([]any(nil), options.Arguments...)
	record := Record{
		ExecutionID: executionID, JobID: jobID, OwnerID: ownerID,
		ProfileHash: profileHash, Entrypoint: entrypoint, WorkerID: workerID,
		ReleaseID: options.ReleaseID, State: "STARTING", Detached: options.Detached,
		Reuse: reuse, Timeout: timeout, Parallelism: parallelism, Permissions: permissions,
		DatabaseAccess: databaseAccess, CheckModules: append([]string(nil), options.CheckModules...),
	}
	return submission{
		record: record, profile: profile, groupKey: options.GroupKey,
		namespace: options.Namespace, arguments: arguments, secrets: copySecrets(options.Secrets),
	}, nil
}

func (m *Manager) runPrepared(ctx context.Context, prepared submission) (Record, error) {
	bounded, cancel := context.WithTimeout(ctx, prepared.record.Timeout)
	defer cancel()
	if prepared.record.State == "QUEUED" {
		return m.awaitQueued(bounded, prepared)
	}
	return m.start(bounded, prepared)
}

func (m *Manager) start(ctx context.Context, prepared submission) (Record, error) {
	record := prepared.record
	m.mu.Lock()
	current, ok := m.records[record.ExecutionID]
	if !ok {
		m.mu.Unlock()
		return record, errors.New("job execution is no longer active")
	}
	if current.State != "STARTING" {
		m.mu.Unlock()
		return current, fmt.Errorf("job execution entered %s before startup", current.State)
	}
	record = current
	if record.Reuse {
		if reusable, ok := m.reusableLocked(record); ok {
			m.stopIdleTimerLocked(reusable.ExecutionID)
			delete(m.records, reusable.ExecutionID)
			record.RuntimeGroupID, record.SandboxID, record.WorkerID = reusable.RuntimeGroupID, reusable.SandboxID, reusable.WorkerID
			record.State = "RUNNING"
			m.records[record.ExecutionID] = record
			m.mu.Unlock()
			prepared.record = record
			return m.execute(ctx, prepared)
		}
	}
	m.mu.Unlock()

	group, err := m.coordinator.Ensure(ctx, coordinator.Request{
		WorkloadType: model.WorkloadJob, OwnerID: record.OwnerID,
		AllocationID: record.WorkerID,
		ExecutionID:  record.ExecutionID, Namespace: prepared.namespace,
		ExplicitGroupKey: prepared.groupKey, Strategy: m.policy.Strategy,
		Profile: prepared.profile, ResourceLimits: m.policy.Resources, Lifecycle: m.policy.Lifecycle,
	})
	if err != nil {
		return m.fail(record, err)
	}
	record.RuntimeGroupID, record.SandboxID = group.Spec.RuntimeGroupID, group.Spec.SandboxID
	m.mu.Lock()
	current, ok = m.records[record.ExecutionID]
	if !ok || current.State != "STARTING" {
		m.mu.Unlock()
		cleanupErr := m.release(record)
		if ok {
			return current, errors.Join(fmt.Errorf("job execution entered %s before Worker startup", current.State), cleanupErr)
		}
		return record, errors.Join(errors.New("job execution is no longer active"), cleanupErr)
	}
	m.mu.Unlock()
	started, err := m.workers.Start(ctx, group.Spec.RuntimeGroupID, supervisor.StartWorkerRequest{
		Metadata: supervisor.ExecutionMetadata{
			WorkerID: record.WorkerID, ExecutionID: record.ExecutionID,
			WorkloadType: model.WorkloadJob, OwnerID: record.OwnerID, WorkloadID: record.JobID,
			ReleaseID: record.ReleaseID, Entrypoint: record.Entrypoint,
			DebuggerName:   "job:" + record.OwnerID + ":" + record.ExecutionID + ":" + record.WorkerID,
			DatabaseAccess: record.DatabaseAccess,
		}, Permissions: record.Permissions,
	})
	if err != nil {
		return m.fail(record, errors.Join(err, m.release(record)))
	}
	record.WorkerID = started.Worker.WorkerID
	m.mu.Lock()
	current, ok = m.records[record.ExecutionID]
	if !ok || current.State != "STARTING" {
		m.mu.Unlock()
		cleanupErr := m.stopAndRelease(record, true)
		if ok {
			return current, errors.Join(fmt.Errorf("job execution entered %s during Worker startup", current.State), cleanupErr)
		}
		return record, errors.Join(errors.New("job execution is no longer active"), cleanupErr)
	}
	record.State = "RUNNING"
	m.records[record.ExecutionID] = liveRecord(record)
	m.mu.Unlock()
	prepared.record = record
	return m.execute(ctx, prepared)
}

func (m *Manager) awaitQueued(ctx context.Context, prepared submission) (Record, error) {
	for {
		m.mu.Lock()
		current, ok := m.records[prepared.record.ExecutionID]
		if !ok {
			m.mu.Unlock()
			return prepared.record, errors.New("job execution is no longer active")
		}
		if current.State != "QUEUED" {
			m.mu.Unlock()
			if current.State == "CANCELLED" {
				return current, context.Canceled
			}
			return current, nil
		}
		if m.closed {
			current.State = "CANCELLED"
			current.FinishedAt = m.now()
			m.records[current.ExecutionID] = liveRecord(current)
			m.signalQueueLocked()
			m.mu.Unlock()
			return current, errors.New("job manager is closed")
		}
		head := current.ExecutionID
		for _, item := range m.records {
			if item.State == "QUEUED" && queueLess(item, current) {
				head = item.ExecutionID
			}
		}
		if head == current.ExecutionID && m.canStartLocked(current) {
			current.State = "STARTING"
			current.StartedAt = m.now()
			if current.StartedAt.Before(current.QueuedAt) {
				current.StartedAt = current.QueuedAt
			}
			m.records[current.ExecutionID] = current
			m.signalQueueLocked()
			m.mu.Unlock()
			prepared.record = current
			return m.start(ctx, prepared)
		}
		change := m.queueChange
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			_ = m.Cancel(context.Background(), current.ExecutionID)
			return m.recordOr(current), ctx.Err()
		case <-change:
		}
	}
}

func queueLess(left, right Record) bool {
	if left.QueuedAt.Equal(right.QueuedAt) {
		return left.ExecutionID < right.ExecutionID
	}
	return left.QueuedAt.Before(right.QueuedAt)
}

func (m *Manager) execute(ctx context.Context, prepared submission) (Record, error) {
	record := prepared.record
	result, err := m.workers.RunJob(ctx, record.WorkerID, prepared.arguments, prepared.secrets, record.CheckModules)
	if err != nil {
		return m.failAndStop(record, redactError(err, prepared.secrets))
	}
	record.Result = redactValue(result.Result, prepared.secrets)
	record.Logs = redactLogs(result.Logs, prepared.secrets)
	record.ModuleDependencies = cloneDependencies(result.ModuleDependencies)
	record.FinishedAt = m.now()
	record.Duration = record.FinishedAt.Sub(record.StartedAt)
	if record.Reuse {
		record.State = "IDLE"
	} else {
		record.State = "SUCCEEDED"
		if cleanupErr := m.stopAndRelease(record, false); cleanupErr != nil {
			return m.fail(record, fmt.Errorf("clean up completed job runtime: %w", cleanupErr))
		}
	}
	m.mu.Lock()
	current, ok := m.records[record.ExecutionID]
	if ok && current.State != "RUNNING" {
		m.mu.Unlock()
		return current, nil
	}
	if record.State == "IDLE" {
		m.records[record.ExecutionID] = liveRecord(record)
		m.scheduleIdleTimerLocked(record)
	} else {
		m.records[record.ExecutionID] = liveRecord(record)
	}
	m.signalQueueLocked()
	m.mu.Unlock()
	return record, nil
}

func (m *Manager) List() ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Record, 0, len(m.records))
	for _, record := range m.records {
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

func (m *Manager) Inspect(executionID string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[executionID]
	if !ok {
		return Record{}, fmt.Errorf("job execution %q not found", executionID)
	}
	return record, nil
}

func (m *Manager) Cancel(ctx context.Context, executionID string) error {
	m.mu.Lock()
	record, ok := m.records[executionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("job execution %q not found", executionID)
	}
	if record.State != "QUEUED" && record.State != "STARTING" && record.State != "RUNNING" {
		m.mu.Unlock()
		return nil
	}
	m.stopIdleTimerLocked(executionID)
	m.mu.Unlock()
	if record.State == "RUNNING" {
		if err := m.stopAndRelease(record, true); err != nil {
			return err
		}
	}
	record.State = "CANCELLED"
	record.FinishedAt = m.now()
	m.mu.Lock()
	if _, exists := m.records[executionID]; exists {
		m.records[executionID] = liveRecord(record)
		m.signalQueueLocked()
	}
	m.mu.Unlock()
	return nil
}

// FailGroup marks live executions failed and retires live idle capacity.
func (m *Manager) FailGroup(runtimeGroupID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for id, record := range m.records {
		if record.RuntimeGroupID != runtimeGroupID {
			continue
		}
		switch record.State {
		case "STARTING", "RUNNING":
			record.State, record.Failure = "FAILED", reason
			record.FinishedAt = m.now()
			record.Duration = record.FinishedAt.Sub(record.StartedAt)
			m.records[id] = liveRecord(record)
			changed = true
		case "IDLE":
			m.stopIdleTimerLocked(id)
			delete(m.records, id)
			changed = true
		}
	}
	if changed {
		m.signalQueueLocked()
	}
	return nil
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
	m.signalQueueLocked()
	for id := range m.timers {
		m.stopIdleTimerLocked(id)
	}
	m.mu.Unlock()
	m.wait.Wait()
	m.mu.Lock()
	idle := make([]Record, 0, len(m.records))
	for id, record := range m.records {
		if record.State == "IDLE" {
			idle = append(idle, record)
			delete(m.records, id)
		}
	}
	m.mu.Unlock()
	var joined error
	for _, record := range idle {
		joined = errors.Join(joined, m.stopAndRelease(record, false))
	}
	return joined
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
	delete(m.timers, executionID)
	record, ok := m.records[executionID]
	if !ok || record.State != "IDLE" || !record.Reuse {
		m.mu.Unlock()
		return
	}
	delete(m.records, executionID)
	m.mu.Unlock()
	if err := m.stopAndRelease(record, false); err != nil && m.logger != nil {
		m.logger.Warn("retire idle job Worker", "execution_id", executionID, "error", err)
	}
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

func (m *Manager) reusableLocked(wanted Record) (Record, bool) {
	for _, item := range m.records {
		if item.JobID == wanted.JobID && item.OwnerID == wanted.OwnerID && item.ReleaseID == wanted.ReleaseID && item.ProfileHash == wanted.ProfileHash && item.DatabaseAccess == wanted.DatabaseAccess && reflect.DeepEqual(item.Permissions, wanted.Permissions) && item.Entrypoint == wanted.Entrypoint && item.State == "IDLE" && item.Reuse {
			return item, true
		}
	}
	return Record{}, false
}

func (m *Manager) fail(record Record, cause error) (Record, error) {
	record.State = "FAILED"
	record.Failure = cause.Error()
	record.FinishedAt = m.now()
	record.Duration = record.FinishedAt.Sub(record.StartedAt)
	m.mu.Lock()
	if current, ok := m.records[record.ExecutionID]; ok && current.State != "STARTING" && current.State != "RUNNING" {
		record = current
	} else if ok {
		m.records[record.ExecutionID] = liveRecord(record)
		m.signalQueueLocked()
	}
	m.mu.Unlock()
	return record, cause
}

func (m *Manager) failAndStop(record Record, cause error) (Record, error) {
	return m.fail(record, errors.Join(cause, m.stopAndRelease(record, true)))
}

func (m *Manager) stopAndRelease(record Record, immediate bool) error {
	if record.RuntimeGroupID == "" {
		return nil
	}
	stopErr := m.workers.StopInGroup(context.Background(), record.RuntimeGroupID, record.WorkerID, immediate)
	return errors.Join(stopErr, m.release(record))
}

func (m *Manager) release(record Record) error {
	if record.RuntimeGroupID == "" {
		return nil
	}
	return m.coordinator.Release(context.Background(), record.RuntimeGroupID, record.WorkerID, "")
}

func (m *Manager) canStartLocked(record Record) bool {
	active, matching := 0, 0
	for _, item := range m.records {
		if item.State != "STARTING" && item.State != "RUNNING" {
			continue
		}
		if item.ExecutionID == record.CallerExecutionID {
			continue
		}
		active++
		if item.JobID == record.JobID {
			matching++
		}
	}
	return active < m.policy.MaximumParallel && matching < record.Parallelism
}

func (m *Manager) queuedCountLocked() int {
	count := 0
	for _, item := range m.records {
		if item.State == "QUEUED" {
			count++
		}
	}
	return count
}

func (m *Manager) nextQueuedTimeLocked() time.Time {
	queued := m.now()
	if !queued.After(m.lastQueued) {
		queued = m.lastQueued.Add(time.Nanosecond)
	}
	m.lastQueued = queued
	return queued
}

func (m *Manager) recordOr(fallback Record) Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.records[fallback.ExecutionID]; ok {
		return record
	}
	return fallback
}

func (m *Manager) removeTerminal(executionID string) {
	m.mu.Lock()
	if record, ok := m.records[executionID]; ok && record.State != "QUEUED" && record.State != "STARTING" && record.State != "RUNNING" && record.State != "IDLE" {
		delete(m.records, executionID)
	}
	m.mu.Unlock()
}

// signalQueueLocked broadcasts a state transition to every queued execution.
// The replacement channel lets any number of waiters observe the next change
// without one timer or polling loop per job.
func (m *Manager) signalQueueLocked() {
	close(m.queueChange)
	m.queueChange = make(chan struct{})
}

func liveRecord(record Record) Record {
	// Returned values may contain program output. The live registry never does.
	record.Result = nil
	record.Logs = nil
	record.ModuleDependencies = nil
	return record
}

func copySecrets(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func clearSecrets(values map[string]string) {
	for name := range values {
		delete(values, name)
	}
}

func redactError(err error, secrets map[string]string) error {
	if err == nil {
		return nil
	}
	if len(secrets) == 0 {
		return err
	}
	var response *supervisor.ResponseError
	if errors.As(err, &response) && response.Code != "" {
		redacted := *response
		redacted.Message = redactText(response.Message, secrets)
		if len(response.Details) > 0 {
			redacted.Details = redactValue(response.Details, secrets).(map[string]any)
		}
		return &redacted
	}
	return errors.New(redactText(err.Error(), secrets))
}

func redactLogs(values []supervisor.LogEvent, secrets map[string]string) []supervisor.LogEvent {
	result := make([]supervisor.LogEvent, len(values))
	for index, value := range values {
		result[index] = supervisor.LogEvent{Level: value.Level, Message: redactText(value.Message, secrets)}
		if len(value.Fields) > 0 {
			result[index].Fields = redactValue(value.Fields, secrets).(map[string]any)
		}
	}
	return result
}

func redactValue(value any, secrets map[string]string) any {
	switch typed := value.(type) {
	case string:
		return redactText(typed, secrets)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = redactValue(typed[index], secrets)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for name, item := range typed {
			result[name] = redactValue(item, secrets)
		}
		return result
	default:
		return value
	}
}

func redactText(value string, secrets map[string]string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[secure input]")
		}
	}
	return value
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

func permissionsFor(value model.Permissions) supervisor.WorkerPermissions {
	sys := []string(nil)
	if value.SystemInfo {
		sys = []string{"hostname", "osRelease"}
	}
	return supervisor.WorkerPermissions{
		Read: append([]string(nil), value.ReadPaths...), Write: append([]string(nil), value.WritePaths...),
		Net: append([]string(nil), value.NetworkHosts...), Import: append([]string(nil), value.ImportHosts...),
		Env: append([]string(nil), value.Environment...), Sys: sys,
	}
}
