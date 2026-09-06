package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/logging/records"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/model"
	sandboxnetwork "the8020/kernel/sandbox/network"
	"the8020/kernel/sandbox/state"
)

type fakeBackend struct {
	beforeCreate                      func(model.SandboxSpec)
	rawPaths                          records.RawPaths
	observations                      map[string]backend.Observation
	created, stopped, killed, deleted []string
	labels                            map[string]map[string]string
	createError                       error
	deleteError                       error
	consoleID                         string
	consoleOptions                    backend.ConsoleOptions
	observeCalls                      atomic.Int64
	listCalls                         atomic.Int64
}

type blockingCreateBackend struct {
	*fakeBackend
	entered chan string
	release chan struct{}
	mu      sync.Mutex
}

func (b *blockingCreateBackend) Create(ctx context.Context, spec model.SandboxSpec, paths records.RawPaths) (backend.Observation, error) {
	select {
	case b.entered <- spec.SandboxID:
	case <-ctx.Done():
		return backend.Observation{}, ctx.Err()
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return backend.Observation{}, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.Create(ctx, spec, paths)
}

type fakeConsole struct {
	done   chan struct{}
	closed bool
}

func (f *fakeConsole) Read([]byte) (int, error)                          { return 0, io.EOF }
func (f *fakeConsole) Write(data []byte) (int, error)                    { return len(data), nil }
func (f *fakeConsole) CloseWrite() error                                 { return nil }
func (f *fakeConsole) Stderr() io.Reader                                 { return nil }
func (f *fakeConsole) Resize(context.Context, backend.ConsoleSize) error { return nil }
func (f *fakeConsole) Done() <-chan struct{}                             { return f.done }
func (f *fakeConsole) Close() error {
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeBackend) Create(_ context.Context, spec model.SandboxSpec, paths records.RawPaths) (backend.Observation, error) {
	f.rawPaths = paths
	if f.beforeCreate != nil {
		f.beforeCreate(spec)
	}
	f.created = append(f.created, spec.SandboxID)
	if f.createError != nil {
		return backend.Observation{}, f.createError
	}
	observation := backend.Observation{Runtime: "io.containerd.runsc.v1", SandboxID: spec.SandboxID, TaskStatus: "running", TaskPID: 42}
	if f.observations == nil {
		f.observations = map[string]backend.Observation{}
	}
	f.observations[spec.SandboxID] = observation
	return observation, nil
}
func (f *fakeBackend) UpdateLabels(_ context.Context, id string, labels map[string]string) error {
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	f.labels[id] = labels
	return nil
}
func (f *fakeBackend) Observe(_ context.Context, id string) (backend.Observation, error) {
	f.observeCalls.Add(1)
	observation, ok := f.observations[id]
	if !ok {
		return observation, os.ErrNotExist
	}
	return observation, nil
}
func (f *fakeBackend) List(context.Context) ([]backend.Observation, error) {
	f.listCalls.Add(1)
	result := make([]backend.Observation, 0, len(f.observations))
	for _, observation := range f.observations {
		result = append(result, observation)
	}
	return result, nil
}
func (f *fakeBackend) ListOwned(ctx context.Context) ([]backend.Observation, error) {
	return f.List(ctx)
}
func (f *fakeBackend) Stop(_ context.Context, id string, _ time.Duration) error {
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeBackend) Kill(_ context.Context, id string) error {
	f.killed = append(f.killed, id)
	return nil
}
func (f *fakeBackend) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	if f.deleteError != nil {
		return f.deleteError
	}
	delete(f.observations, id)
	return nil
}
func (f *fakeBackend) OpenConsole(_ context.Context, id string, options backend.ConsoleOptions) (backend.Console, error) {
	f.consoleID = id
	f.consoleOptions = options
	return &fakeConsole{done: make(chan struct{})}, nil
}

type fakeNetwork struct {
	allocated, checked, released []string
	allocationError              error
}

type serializedNetwork struct {
	*fakeNetwork
	mu sync.Mutex
}

func (n *serializedNetwork) Allocate(ctx context.Context, sandboxID string, configuration model.NetworkConfiguration) (sandboxnetwork.Allocation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.fakeNetwork.Allocate(ctx, sandboxID, configuration)
}

func (f *fakeNetwork) Allocate(_ context.Context, sandboxID string, _ model.NetworkConfiguration) (sandboxnetwork.Allocation, error) {
	f.allocated = append(f.allocated, sandboxID)
	if f.allocationError != nil {
		return sandboxnetwork.Allocation{}, f.allocationError
	}
	return sandboxnetwork.Allocation{SandboxID: sandboxID, NamespaceName: "ns-" + sandboxID, NamespacePath: "/var/run/netns/ns-" + sandboxID, IPs: []string{"10.88.0.4"}}, nil
}
func (f *fakeNetwork) Check(_ context.Context, group string) error {
	f.checked = append(f.checked, group)
	return nil
}
func (f *fakeNetwork) Release(_ context.Context, group string) error {
	f.released = append(f.released, group)
	return nil
}

type fakeSupervisor struct {
	status      supervisor.Status
	workers     []supervisor.WorkerStatus
	statusError error
	drains      int
	workerCalls int
}

func (f *fakeSupervisor) Status(_ context.Context, spec model.SandboxSpec) (supervisor.Status, error) {
	if f.statusError != nil {
		return supervisor.Status{}, f.statusError
	}
	status := f.status
	if status.Revision == 0 {
		status.Revision = 1
	}
	status.ProtocolVersion, status.SandboxID, status.WorkloadType = 1, spec.SandboxID, spec.WorkloadType
	return status, nil
}
func (f *fakeSupervisor) Workers(context.Context, model.SandboxSpec) ([]supervisor.WorkerStatus, error) {
	f.workerCalls++
	return append([]supervisor.WorkerStatus(nil), f.workers...), nil
}
func (f *fakeSupervisor) Snapshot(ctx context.Context, spec model.SandboxSpec) (model.RuntimeSnapshot, error) {
	status, err := f.Status(ctx, spec)
	if err != nil {
		return model.RuntimeSnapshot{}, err
	}
	workers, err := f.Workers(ctx, spec)
	if err != nil {
		return model.RuntimeSnapshot{}, err
	}
	snapshot := model.RuntimeSnapshot{
		Revision: status.Revision, ProtocolVersion: status.ProtocolVersion,
		SupervisorVersion: status.SupervisorVersion, DenoVersion: status.DenoVersion,
		SandboxID:    status.SandboxID,
		WorkloadType: status.WorkloadType, WorkerCount: len(workers),
	}
	for _, worker := range workers {
		snapshot.Workers = append(snapshot.Workers, model.RuntimeWorkerStatus{WorkerID: worker.WorkerID, WorkloadID: worker.WorkloadID, State: worker.State})
	}
	return snapshot, nil
}
func (f *fakeSupervisor) Drain(context.Context, model.SandboxSpec) error { f.drains++; return nil }

type fakePorts struct {
	closed []string
	err    error
}

type fakeLogRegistry struct {
	mu               sync.Mutex
	registered       map[string]string
	beforeUnregister func(string)
	position         string
}

func (f *fakeLogRegistry) ReadPosition() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.position
}

func (f *fakeLogRegistry) RegisterSandbox(_ context.Context, id, token string) (records.RawPaths, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered[id] = token
	return records.RawPaths{Stdout: "/logs/" + id + "-stdout", Stderr: "/logs/" + id + "-stderr"}, nil
}
func (f *fakeLogRegistry) UnregisterSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeUnregister != nil {
		f.beforeUnregister(id)
	}
	delete(f.registered, id)
	return nil
}

func (f *fakePorts) CloseForSandbox(sandboxID string) error {
	f.closed = append(f.closed, sandboxID)
	return f.err
}

func TestCreateInspectStopDeleteLifecycle(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, runtimeSupervisor := testManager(t)
	runtimePorts := manager.ports.(*fakePorts)
	inspection, err := manager.Create(context.Background(), testSandboxSpec(t, "sandbox-one"))
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status.ObservedState != model.StateReady || !inspection.Status.SupervisorHealthy || inspection.Status.TaskPID != 42 || inspection.Spec.Network.SandboxIP != "10.88.0.4" {
		t.Fatalf("inspection: %#v", inspection)
	}
	workerCalls := runtimeSupervisor.workerCalls
	resolved, err := manager.ResolveSandbox("sandbox-one")
	if err != nil || resolved.SandboxID != "sandbox-one" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if runtimeSupervisor.workerCalls != workerCalls {
		t.Fatal("sandbox resolution contacted the supervisor")
	}
	runtimeSupervisor.workers = []supervisor.WorkerStatus{{WorkerID: "worker-one", State: "ready"}}
	inspected, err := manager.Inspect(context.Background(), "sandbox-one")
	if err != nil || len(inspected.Workers) != 0 || runtimeSupervisor.workerCalls != workerCalls {
		t.Fatalf("cached inspect=%#v calls=%d err=%v", inspected, runtimeSupervisor.workerCalls, err)
	}
	inspected, err = manager.Refresh(context.Background(), "sandbox-one")
	if err != nil || len(inspected.Workers) != 1 || inspected.Status.WorkerCount != 1 {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	consoleOptions := backend.ConsoleOptions{
		Arguments: []string{"/bin/bash", "-l"}, Environment: []string{"TERM=xterm-256color"},
		WorkingDir: "/", Size: backend.ConsoleSize{Columns: 80, Rows: 24},
	}
	console, err := manager.OpenConsole(context.Background(), "sandbox-one", consoleOptions)
	if err != nil {
		t.Fatal(err)
	}
	_ = console.Close()
	if runtimeBackend.consoleID != "sandbox-one" || runtimeBackend.consoleOptions.WorkingDir != "/" || runtimeBackend.consoleOptions.Size != consoleOptions.Size {
		t.Fatalf("console open = id %q options %#v", runtimeBackend.consoleID, runtimeBackend.consoleOptions)
	}
	if err := manager.Stop(context.Background(), "sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.OpenConsole(context.Background(), "sandbox-one", consoleOptions); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("stopped sandbox console error = %v", err)
	}
	_, status, err := store.Load("sandbox-one")
	if err != nil || status.ObservedState != model.StateStopped || runtimeSupervisor.drains != 1 || len(runtimeBackend.stopped) != 1 || !contains(runtimePorts.closed, "sandbox-one") {
		t.Fatalf("stopped status=%#v err=%v backend=%#v ports=%#v drains=%d", status, err, runtimeBackend.stopped, runtimePorts.closed, runtimeSupervisor.drains)
	}
	workerCalls = runtimeSupervisor.workerCalls
	inspected, err = manager.Inspect(context.Background(), "sandbox-one")
	if err != nil || inspected.Status.ObservedState != model.StateStopped || len(inspected.Workers) != 0 {
		t.Fatalf("terminal inspect=%#v err=%v", inspected, err)
	}
	if runtimeSupervisor.workerCalls != workerCalls {
		t.Fatal("terminal sandbox inspection contacted its stopped supervisor")
	}
	if err := manager.Delete(context.Background(), "sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if len(runtimeBackend.deleted) != 1 || len(runtimeNetwork.released) != 1 {
		t.Fatalf("cleanup backend=%#v network=%#v", runtimeBackend.deleted, runtimeNetwork.released)
	}
	if _, _, err := store.Load("sandbox-one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state remains: %v", err)
	}
}

func TestLogSourceRegistrationPrecedesNativeStartAndSurvivesFailedCleanup(t *testing.T) {
	manager, _, runtimeBackend, _, _ := testManager(t)
	registry := manager.logs.(*fakeLogRegistry)
	spec := testSandboxSpec(t, "sandbox-logging")
	runtimeBackend.beforeCreate = func(created model.SandboxSpec) {
		if registry.registered[created.SandboxID] != created.InternalToken {
			t.Fatal("native startup preceded trusted log registration")
		}
		if runtimeBackend.rawPaths != (records.RawPaths{Stdout: "/logs/" + created.SandboxID + "-stdout", Stderr: "/logs/" + created.SandboxID + "-stderr"}) {
			t.Fatal("native startup did not receive the registered raw endpoints")
		}
	}
	registry.beforeUnregister = func(id string) {
		if _, exists := runtimeBackend.observations[id]; exists {
			t.Fatal("log registration was released before native deletion")
		}
	}
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runtimeBackend.deleteError = errors.New("native cleanup failed")
	if err := manager.Delete(context.Background(), spec.SandboxID); err == nil {
		t.Fatal("expected cleanup failure")
	}
	if registry.registered[spec.SandboxID] != spec.InternalToken {
		t.Fatal("failed cleanup lost live producer binding")
	}
	runtimeBackend.deleteError = nil
	if err := manager.Delete(context.Background(), spec.SandboxID); err != nil {
		t.Fatal(err)
	}
	if len(registry.registered) != 0 {
		t.Fatal("retired log binding was retained")
	}
}

func TestFailedCreationReleasesLogBindingAndReconciliationRestoresExistingToken(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	registry := manager.logs.(*fakeLogRegistry)
	registry.position = "before-native-start"
	runtimeBackend.beforeCreate = func(spec model.SandboxSpec) {
		_, status, err := store.Load(spec.SandboxID)
		if err != nil || status.NodeID != manager.instanceUUID || status.CreatedAt.IsZero() || status.LogPosition != "before-native-start" {
			t.Fatalf("log reference was not persisted before native startup: %#v, %v", status, err)
		}
	}
	spec := testSandboxSpec(t, "sandbox-logging-failure")
	runtimeBackend.createError = errors.New("start failed")
	if _, err := manager.Create(context.Background(), spec); err == nil {
		t.Fatal("expected startup failure")
	}
	if len(registry.registered) != 0 {
		t.Fatal("creation rollback retained its log binding")
	}
	failed := historyForSandbox(t, manager, spec.SandboxID).Record.Status
	if failed.LogPosition != "before-native-start" || failed.CreatedAt.IsZero() || !failed.StartedAt.IsZero() {
		t.Fatalf("native startup failure lost its reference: %#v", failed)
	}
	runtimeBackend.createError = nil
	spec = testSandboxSpec(t, "sandbox-logging-restored")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	registry.registered = map[string]string{}
	if report, err := manager.Reconcile(context.Background()); err != nil || report.Restored != 1 {
		t.Fatal(report, err)
	}
	if registry.registered[spec.SandboxID] != spec.InternalToken {
		t.Fatal("restoration changed or omitted producer credentials")
	}
}

func TestManagerAllocatesCompactSandboxIDsAndRejectsRetainedCollision(t *testing.T) {
	manager, _, _, _, _ := testManager(t)
	id, err := manager.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	if matched, _ := regexp.MatchString(`^sbx-[a-z0-9]{10}$`, id); !matched {
		t.Fatalf("sandbox ID = %q", id)
	}
	spec := testSandboxSpec(t, id)
	if _, err := manager.history.Archive(spec, model.SandboxStatus{ObservedState: model.StateFailed}, "failed", manager.historyRetention); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "retained in history") {
		t.Fatalf("retained collision error = %v", err)
	}
}

func TestCreateFailureRollsBackAndArchivesFailure(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, runtimeSupervisor := testManager(t)
	manager.logs.(*fakeLogRegistry).position = "before-sandbox-startup"
	runtimeSupervisor.statusError = errors.New("supervisor offline")
	manager.startupTimeout = 5 * time.Millisecond
	manager.probeInterval = time.Millisecond
	inspection, err := manager.Create(context.Background(), testSandboxSpec(t, "sandbox-failed"))
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("create error=%v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create error lost readiness deadline: %v", err)
	}
	if inspection.Spec.SandboxID != "sandbox-failed" {
		t.Fatalf("create error lost allocated sandbox identity: %#v", inspection)
	}
	if _, _, loadErr := store.Load("sandbox-failed"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("failed sandbox remains live: %v", loadErr)
	}
	archived := historyForSandbox(t, manager, "sandbox-failed")
	if archived.Record.Status.ObservedState != model.StateFailed || archived.Record.Spec.InternalToken != "" || !strings.Contains(archived.Record.Status.FailureReason, "supervisor offline") {
		t.Fatalf("history=%#v", archived)
	}
	if archived.Record.Status.NodeID != manager.instanceUUID || !archived.Record.Status.CreatedAt.Equal(manager.now()) || archived.Record.Status.LogPosition != "before-sandbox-startup" {
		t.Fatalf("failed startup lost its log reference: %#v", archived.Record.Status)
	}
	if len(runtimeBackend.deleted) != 1 || len(runtimeNetwork.released) != 1 {
		t.Fatalf("rollback backend=%#v network=%#v", runtimeBackend.deleted, runtimeNetwork.released)
	}
}

func TestAssignWarmRequiresCleanHealthyGroupAndPersistsOwner(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	manager.logs.(*fakeLogRegistry).position = "before-warm-startup"
	spec := testSandboxSpec(t, "sandbox-warm")
	spec.GroupKey = ""
	spec.OwnerIDs = nil
	spec.Lifecycle.Warm = true
	inspection, err := manager.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := manager.AssignWarm(context.Background(), inspection.Spec.SandboxID, "job:owner:nightly", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Spec.Lifecycle.Warm || assigned.Spec.GroupKey != "job:owner:nightly" || len(assigned.Spec.OwnerIDs) != 1 || assigned.Spec.OwnerIDs[0] != "nightly" || assigned.Status.CurrentOwners[0] != "nightly" {
		t.Fatalf("assigned=%#v", assigned)
	}
	if assigned.Status.NodeID != manager.instanceUUID || assigned.Status.LogPosition != "before-warm-startup" || !assigned.Status.CreatedAt.Equal(inspection.Status.CreatedAt) {
		t.Fatalf("warm assignment changed the sandbox log reference: %#v", assigned.Status)
	}
	stored, status, err := store.Load("sandbox-warm")
	if err != nil || stored.Lifecycle.Warm || status.CurrentOwners[0] != "nightly" || runtimeBackend.labels["sandbox-warm"]["the8020.owner"] != "nightly" {
		t.Fatalf("stored=%#v status=%#v labels=%#v err=%v", stored, status, runtimeBackend.labels, err)
	}
	if _, err := manager.AssignWarm(context.Background(), "sandbox-warm", "job:owner:other", "other"); err == nil {
		t.Fatal("assigned warm group was reusable")
	}
}

func TestAddOwnerPersistsSharedGroupOwnershipAndContainerLabel(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-shared")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	updated, err := manager.AddOwner(context.Background(), spec.SandboxID, "second-owner")
	if err != nil {
		t.Fatal(err)
	}
	stored, status, err := store.Load(spec.SandboxID)
	if err != nil || len(updated.Spec.OwnerIDs) != 2 || updated.Spec.OwnerIDs[1] != "second-owner" || len(status.CurrentOwners) != 2 || len(stored.OwnerIDs) != 2 || runtimeBackend.labels[spec.SandboxID]["the8020.owners"] != "job,second-owner" {
		t.Fatalf("updated=%#v stored=%#v status=%#v labels=%#v err=%v", updated, stored, status, runtimeBackend.labels, err)
	}
	if _, exists := runtimeBackend.labels[spec.SandboxID]["the8020.services"]; exists {
		t.Fatalf("job ownership update emitted an empty service label: %#v", runtimeBackend.labels)
	}
	if _, err := manager.AddOwner(context.Background(), spec.SandboxID, "second-owner"); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = store.Load(spec.SandboxID)
	if len(stored.OwnerIDs) != 2 {
		t.Fatalf("idempotent owner add duplicated owners: %#v", stored.OwnerIDs)
	}
}

func TestRemoveOwnerRetainsSharedSandboxThenDeletesItWhenEmpty(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-shared")
	spec.WorkloadType = model.WorkloadService
	spec.RuntimeProfile.WorkloadType = model.WorkloadService
	profileHash, err := spec.RuntimeProfile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	spec.ProfileHash = profileHash
	spec.OwnerIDs = []string{"replica-a"}
	spec.ServiceIDs = []string{"service-a"}
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddOwner(context.Background(), spec.SandboxID, "replica-b", "service-b"); err != nil {
		t.Fatal(err)
	}
	destroyed, err := manager.RemoveOwner(context.Background(), spec.SandboxID, "replica-a", "service-a")
	if err != nil || destroyed {
		t.Fatalf("destroyed=%v err=%v", destroyed, err)
	}
	stored, status, err := store.Load(spec.SandboxID)
	if err != nil || len(stored.OwnerIDs) != 1 || stored.OwnerIDs[0] != "replica-b" || len(stored.ServiceIDs) != 1 || stored.ServiceIDs[0] != "service-b" || len(status.CurrentOwners) != 1 || runtimeBackend.labels[spec.SandboxID]["the8020.owners"] != "replica-b" || runtimeBackend.labels[spec.SandboxID]["the8020.services"] != "service-b" {
		t.Fatalf("stored=%#v status=%#v labels=%#v err=%v", stored, status, runtimeBackend.labels, err)
	}
	destroyed, err = manager.RemoveOwner(context.Background(), spec.SandboxID, "replica-b", "service-b")
	if err != nil || !destroyed {
		t.Fatalf("destroyed=%v err=%v", destroyed, err)
	}
	if _, _, err := store.Load(spec.SandboxID); !errors.Is(err, os.ErrNotExist) || len(runtimeBackend.deleted) != 1 || len(runtimeNetwork.released) != 1 {
		t.Fatalf("load err=%v deleted=%#v released=%#v", err, runtimeBackend.deleted, runtimeNetwork.released)
	}
}

func TestSandboxAdmissionEnforcesCountAndTemporaryStorageBudgets(t *testing.T) {
	manager, _, _, _, _ := testManager(t)
	manager.nodeLimits = NodeLimits{MaximumSandboxes: 1, TemporaryStorageBytes: 100}
	first := testSandboxSpec(t, "sandbox-first")
	first.ResourceLimits.TmpfsMaximum = 64
	if _, err := manager.Create(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testSandboxSpec(t, "sandbox-second")
	if _, err := manager.Create(context.Background(), second); err == nil || !strings.Contains(err.Error(), "sandbox capacity") {
		t.Fatalf("second sandbox admission error=%v", err)
	}
	capacity, err := manager.Capacity()
	if err != nil || capacity.SandboxCount != 1 || capacity.TemporaryStorageBytes != 64 {
		t.Fatalf("capacity=%#v err=%v", capacity, err)
	}
}

func TestUnrelatedSandboxCreationsDoNotSerializeSlowBackendIO(t *testing.T) {
	manager, _, runtimeBackend, runtimeNetwork, _ := testManager(t)
	entered := make(chan string, 2)
	release := make(chan struct{})
	manager.backend = &blockingCreateBackend{fakeBackend: runtimeBackend, entered: entered, release: release}
	manager.network = &serializedNetwork{fakeNetwork: runtimeNetwork}

	results := make(chan error, 2)
	for _, identity := range []string{"sandbox-parallel-a", "sandbox-parallel-b"} {
		spec := testSandboxSpec(t, identity)
		go func() {
			_, err := manager.Create(context.Background(), spec)
			results <- err
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case group := <-entered:
			seen[group] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatal("unrelated sandbox creation was serialized behind backend I/O")
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("entered sandboxes=%#v", seen)
	}
}

func TestMetricsReadsSandboxCgroupAndPersistsSnapshot(t *testing.T) {
	manager, store, _, _, _ := testManager(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	manager.now = func() time.Time { return now }
	manager.cgroupRoot = t.TempDir()
	spec := testSandboxSpec(t, "sandbox-metrics")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(manager.cgroupRoot, "the8020", manager.instanceUUID, spec.SandboxID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"memory.current": "10\n", "memory.peak": "12\n", "pids.current": "2\n", "memory.events": "oom 1\n", "pids.events": "max 0\n", "cpu.stat": "usage_usec 33\n", "cgroup.events": "populated 1\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := manager.Metrics(spec.SandboxID)
	if err != nil || metrics.MemoryPeak != 12 || metrics.CPUUsageMicros != 33 || !metrics.SampledAt.Equal(now) {
		t.Fatalf("metrics=%#v err=%v", metrics, err)
	}
	now = now.Add(time.Second)
	if err := os.WriteFile(filepath.Join(directory, "memory.current"), []byte("200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cpu.stat"), []byte("usage_usec 500033\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metrics, err = manager.Metrics(spec.SandboxID)
	if err != nil || metrics.CPUUsageMicros != 500033 || metrics.MemoryCurrent != 200 || !metrics.SampledAt.Equal(now) {
		t.Fatalf("derived metrics=%#v err=%v", metrics, err)
	}
	_, status, err := store.Load(spec.SandboxID)
	if err != nil || status.Metrics.MemoryCurrent != 200 || status.Metrics.CPUUsageMicros != 500033 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestHealthCheckDetectsOOMPreservesMetricsAndTerminatesGroup(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, _ := testManager(t)
	runtimePorts := manager.ports.(*fakePorts)
	manager.cgroupRoot = t.TempDir()
	spec := testSandboxSpec(t, "sandbox-oom")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(manager.cgroupRoot, "the8020", manager.instanceUUID, spec.SandboxID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"memory.current": "250\n", "memory.peak": "256\n", "pids.current": "2\n", "memory.events": "oom 1\noom_kill 1\noom_group_kill 1\n", "pids.events": "max 0\n", "cpu.stat": "usage_usec 33\n", "cgroup.events": "populated 0\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager.now = func() time.Time { return time.Unix(1_700_000_120, 0).UTC() }
	report, err := manager.CheckHealth(context.Background(), time.Minute)
	if err != nil || report.Checked != 1 || len(report.Failures) != 1 || !report.Failures[0].OOM || !strings.Contains(report.Failures[0].Reason, "OOM") {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, _, err := store.Load(spec.SandboxID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed sandbox remains live: %v", err)
	}
	archived := historyForSandbox(t, manager, spec.SandboxID)
	if archived.Record.Status.ObservedState != model.StateFailed || archived.Record.Status.Metrics.MemoryPeak != 256 || len(runtimeBackend.killed) != 1 || !contains(runtimeBackend.deleted, spec.SandboxID) || !contains(runtimeNetwork.released, spec.SandboxID) || !contains(runtimePorts.closed, spec.SandboxID) {
		t.Fatalf("history=%#v killed=%#v deleted=%#v network=%#v ports=%#v", archived, runtimeBackend.killed, runtimeBackend.deleted, runtimeNetwork.released, runtimePorts.closed)
	}
}

func TestHealthyHeartbeatCheckUsesOnlyCachedState(t *testing.T) {
	manager, _, runtimeBackend, _, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-healthy-cache")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runtimeBackend.observeCalls.Store(0)
	runtimeBackend.listCalls.Store(0)
	report, err := manager.CheckHealth(context.Background(), time.Minute)
	if err != nil || report.Checked != 0 || len(report.Failures) != 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if runtimeBackend.observeCalls.Load() != 0 || runtimeBackend.listCalls.Load() != 0 {
		t.Fatalf("healthy cache check performed backend I/O: observe=%d list=%d", runtimeBackend.observeCalls.Load(), runtimeBackend.listCalls.Load())
	}
}

func TestHealthCheckDetectsSupervisorHeartbeatTimeout(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-timeout")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Unix(1_700_000_120, 0).UTC() }
	report, err := manager.CheckHealth(context.Background(), time.Minute)
	if err != nil || len(report.Failures) != 1 || report.Failures[0].OOM || !strings.Contains(report.Failures[0].Reason, "heartbeat") {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, _, err := store.Load(spec.SandboxID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed sandbox remains live: %v", err)
	}
	archived := historyForSandbox(t, manager, spec.SandboxID)
	if archived.Record.Status.ObservedState != model.StateFailed || len(runtimeBackend.killed) != 1 {
		t.Fatalf("history=%#v killed=%#v", archived, runtimeBackend.killed)
	}
}

func TestHealthCleanupFailureRemainsInLiveCatalog(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-cleanup-failed")
	if _, err := manager.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runtimeBackend.deleteError = errors.New("backend busy")
	manager.now = func() time.Time { return time.Unix(1_700_000_120, 0).UTC() }
	report, err := manager.CheckHealth(context.Background(), time.Minute)
	if err != nil || len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Reason, "backend busy") {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	_, status, err := store.Load(spec.SandboxID)
	if err != nil || status.ObservedState != model.StateFailed {
		t.Fatalf("live failed status=%#v err=%v", status, err)
	}
	items, err := manager.List()
	if err != nil || len(items) != 1 || items[0].Spec.SandboxID != spec.SandboxID {
		t.Fatalf("live items=%#v err=%v", items, err)
	}
	page, err := manager.ListHistory(10, "")
	if err != nil || len(page.Sandboxes) != 0 {
		t.Fatalf("history=%#v err=%v", page, err)
	}
}

func TestReconcileRestoresSandboxesMarksMissingAndDeletesOrphans(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, _ := testManager(t)
	for _, sandboxID := range []string{"sandbox-restored", "sandbox-missing"} {
		spec := testSandboxSpec(t, sandboxID)
		if err := store.SaveSpec(spec); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveStatus(sandboxID, model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err != nil {
			t.Fatal(err)
		}
	}
	runtimeBackend.observations = map[string]backend.Observation{
		"sandbox-restored": {SandboxID: "sandbox-restored", Runtime: "io.containerd.runsc.v1", TaskStatus: "running", TaskPID: 99},
		"orphan":           {SandboxID: "orphan", Runtime: "io.containerd.runsc.v1", TaskStatus: "running"},
	}
	report, err := manager.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Restored != 1 || len(report.Missing) != 1 || report.Missing[0] != "sandbox-missing" || len(report.Terminated) != 1 || report.Terminated[0].SandboxID != "sandbox-missing" || len(report.OrphansDeleted) != 1 || report.OrphansDeleted[0] != "orphan" {
		t.Fatalf("report=%#v", report)
	}
	if _, _, err := store.Load("sandbox-missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing sandbox remains live: %v", err)
	}
	missing := historyForSandbox(t, manager, "sandbox-missing")
	if missing.Record.Status.ObservedState != model.StateFailed || len(runtimeNetwork.checked) != 1 || !contains(runtimeBackend.deleted, "orphan") || !contains(runtimeNetwork.released, "orphan") {
		t.Fatalf("missing=%#v backend=%#v network=%#v", missing, runtimeBackend.deleted, runtimeNetwork)
	}
}

func TestStartupReconcilePreservesHealthySandboxes(t *testing.T) {
	manager, store, runtimeBackend, _, _ := testManager(t)
	spec := testSandboxSpec(t, "sandbox-existing")
	if err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(spec.SandboxID, model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err != nil {
		t.Fatal(err)
	}
	runtimeBackend.observations[spec.SandboxID] = backend.Observation{SandboxID: spec.SandboxID, Runtime: "io.containerd.runsc.v1", TaskStatus: "running", TaskPID: 99}

	report, err := manager.Startup(context.Background(), StartupReconcile)
	if err != nil || report.Restored != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, _, err := store.Load(spec.SandboxID); err != nil || len(runtimeBackend.killed) != 0 || len(runtimeBackend.deleted) != 0 {
		t.Fatalf("existing group was not preserved: killed=%#v deleted=%#v err=%v", runtimeBackend.killed, runtimeBackend.deleted, err)
	}
}

func TestStartupDestroyRemovesKnownSandboxesAndOwnedOrphans(t *testing.T) {
	manager, store, runtimeBackend, runtimeNetwork, runtimeSupervisor := testManager(t)
	runtimeSupervisor.statusError = errors.New("startup destruction must not probe supervisors")
	spec := testSandboxSpec(t, "sandbox-existing")
	if err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(spec.SandboxID, model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err != nil {
		t.Fatal(err)
	}
	runtimeBackend.observations = map[string]backend.Observation{
		spec.SandboxID: {SandboxID: spec.SandboxID, Runtime: "io.containerd.runsc.v1", TaskStatus: "running", TaskPID: 99},
		"orphan":       {SandboxID: "orphan", Runtime: "io.containerd.runsc.v1", TaskStatus: "running", TaskPID: 100},
	}

	report, err := manager.Startup(context.Background(), StartupDestroy)
	if err != nil || report.Restored != 0 || len(report.Terminated) != 1 || report.Terminated[0].SandboxID != spec.SandboxID || len(report.OrphansDeleted) != 1 || report.OrphansDeleted[0] != "orphan" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if ids, err := store.List(); err != nil || len(ids) != 0 {
		t.Fatalf("persisted groups=%#v err=%v", ids, err)
	}
	if len(runtimeBackend.observations) != 0 || len(runtimeBackend.killed) != 0 || !contains(runtimeBackend.deleted, spec.SandboxID) || !contains(runtimeBackend.deleted, "orphan") {
		t.Fatalf("observations=%#v killed=%#v deleted=%#v", runtimeBackend.observations, runtimeBackend.killed, runtimeBackend.deleted)
	}
	if !contains(runtimeNetwork.released, spec.SandboxID) || !contains(runtimeNetwork.released, "orphan") {
		t.Fatalf("released networks=%#v", runtimeNetwork.released)
	}
	if len(runtimeNetwork.checked) != 0 {
		t.Fatalf("startup destruction checked inherited networks: %#v", runtimeNetwork.checked)
	}
}

func TestStartupRejectsUnknownPolicyWithoutSideEffects(t *testing.T) {
	manager, _, runtimeBackend, _, _ := testManager(t)
	runtimeBackend.observations["orphan"] = backend.Observation{SandboxID: "orphan", TaskStatus: "running"}
	if _, err := manager.Startup(context.Background(), StartupPolicy("invalid")); err == nil {
		t.Fatal("unknown startup policy was accepted")
	}
	if len(runtimeBackend.observations) != 1 || len(runtimeBackend.deleted) != 0 {
		t.Fatalf("unknown policy caused side effects: observations=%#v deleted=%#v", runtimeBackend.observations, runtimeBackend.deleted)
	}
}

func testManager(t *testing.T) (*Manager, *state.Store, *fakeBackend, *fakeNetwork, *fakeSupervisor) {
	t.Helper()
	root := t.TempDir()
	store, err := state.New(filepath.Join(root, "sandboxes"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeBackend := &fakeBackend{observations: map[string]backend.Observation{}}
	runtimeNetwork := &fakeNetwork{}
	runtimeSupervisor := &fakeSupervisor{status: supervisor.Status{SupervisorVersion: "test", DenoVersion: "2.9.4"}}
	now := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	historyStore, err := history.New(history.Config{Root: filepath.Join(root, "history"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{InstanceUUID: "instance-one", Store: store, Backend: runtimeBackend, Network: runtimeNetwork, Supervisor: runtimeSupervisor, Ports: &fakePorts{}, Logs: &fakeLogRegistry{registered: map[string]string{}}, History: historyStore, StartupTimeout: 50 * time.Millisecond, ProbeInterval: time.Millisecond, StopGrace: time.Millisecond, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, runtimeBackend, runtimeNetwork, runtimeSupervisor
}

func testSandboxSpec(t *testing.T, sandbox string) model.SandboxSpec {
	t.Helper()
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: digest, DependencyMode: model.DependencyCachedOnly, NetworkMode: "netstack", ResourceClass: "job-default"}
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return model.SandboxSpec{SandboxID: sandbox, WorkloadType: model.WorkloadJob, GroupKey: "job:owner:job", OwnerIDs: []string{"job"}, ImageDigest: digest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: model.ResourceLimits{PIDMaximum: 32, TmpfsMaximum: 64}, Network: model.NetworkConfiguration{Mode: "netstack", NetworkName: "the8020"}, DependencyMode: model.DependencyCachedOnly, Lifecycle: model.LifecyclePolicy{StopGracePeriod: time.Second}, InternalToken: "0123456789abcdef0123456789abcdef"}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func historyForSandbox(t *testing.T, manager *Manager, sandboxID string) history.Inspection {
	t.Helper()
	page, err := manager.ListHistory(history.MaximumPageSize, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Sandboxes {
		if item.SandboxID != sandboxID {
			continue
		}
		inspection, err := manager.InspectHistory(item.HistoryID)
		if err != nil {
			t.Fatal(err)
		}
		return inspection
	}
	t.Fatalf("sandbox %s is missing from history: %#v", sandboxID, page)
	return history.Inspection{}
}
