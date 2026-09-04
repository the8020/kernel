package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/sandbox/model"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testRecord(t *testing.T, id string) (model.SandboxSpec, model.SandboxStatus) {
	t.Helper()
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: testDigest, DependencyMode: model.DependencyCachedOnly, NetworkMode: "netstack", ResourceClass: "job-default"}
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	spec := model.SandboxSpec{SandboxID: "sandbox-" + id, RuntimeGroupID: id, WorkloadType: model.WorkloadJob, GroupKey: "job:one", OwnerIDs: []string{"one"}, ImageDigest: testDigest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: model.ResourceLimits{PIDMaximum: 32, TmpfsMaximum: 64}, Network: model.NetworkConfiguration{Mode: "netstack", NetworkName: "the8020"}, DependencyMode: model.DependencyCachedOnly, InternalToken: "secret-" + id}
	status := model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateCreating}
	return spec, status
}

func TestStorePersistsListsTransitionsAndDeletes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "groups")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"group-b", "group-a"} {
		spec, status := testRecord(t, id)
		if err := store.SaveSpec(spec); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveStatus(id, status); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.List()
	if err != nil || len(ids) != 2 || ids[0] != "group-a" || ids[1] != "group-b" {
		t.Fatalf("List() = %#v, %v", ids, err)
	}
	spec, status, err := store.Load("group-a")
	if err != nil || spec.SandboxID != "sandbox-group-a" || spec.InternalToken != "secret-group-a" || status.ObservedState != model.StateCreating {
		t.Fatalf("Load() = %#v %#v, %v", spec, status, err)
	}
	storedSpec, err := os.ReadFile(filepath.Join(root, "group-a", "spec.json"))
	if err != nil || strings.Contains(string(storedSpec), "secret-group-a") {
		t.Fatalf("secret leaked into specification: %v %s", err, storedSpec)
	}
	status, err = store.Transition("group-a", model.StateStarting, func(value *model.SandboxStatus) { value.ContainerID = "container" })
	if err != nil || status.ContainerID != "container" {
		t.Fatalf("Transition() = %#v, %v", status, err)
	}
	heartbeat := time.Unix(100, 0).UTC()
	status, err = store.UpdateStatus("group-a", func(value *model.SandboxStatus) error {
		value.LastHeartbeat = heartbeat
		return nil
	})
	if err != nil || !status.LastHeartbeat.Equal(heartbeat) {
		t.Fatalf("UpdateStatus() = %#v, %v", status, err)
	}
	status, transitioned, err := store.TransitionIf("group-a", model.StateFailed, func(current model.SandboxStatus) bool {
		return current.LastHeartbeat.Before(heartbeat)
	}, nil)
	if err != nil || transitioned || status.ObservedState != model.StateStarting {
		t.Fatalf("conditional transition = %#v %t, %v", status, transitioned, err)
	}
	if _, err := store.Transition("group-a", model.StateActive, nil); err == nil || !strings.Contains(err.Error(), "invalid sandbox transition") {
		t.Fatalf("illegal transition error = %v", err)
	}
	if err := store.Delete("group-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("group-b"); !os.IsNotExist(err) {
		t.Fatalf("deleted group load error = %v", err)
	}
	for _, path := range []string{root, filepath.Join(root, "group-a"), filepath.Join(root, "group-a", "spec.json"), filepath.Join(root, "group-a", "state.json"), filepath.Join(root, "group-a", "secret.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if !info.IsDir() {
			want = 0o600
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestStoreRejectsCorruptionAndUnsafeIDs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "groups")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus("../escape", model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err == nil {
		t.Fatal("accepted unsafe runtime-group ID")
	}
	directory := filepath.Join(root, "broken")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(directory, "spec.json"), []byte("not json"), 0o600)
	_ = os.WriteFile(filepath.Join(directory, "state.json"), []byte("{}"), 0o600)
	if _, _, err := store.Load("broken"); err == nil || !strings.Contains(err.Error(), "decode spec.json") {
		t.Fatalf("corrupt load error = %v", err)
	}
}

func TestCachedLookupNeverFallsBackToRecoveryFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "groups")
	cache, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	spec, status := testRecord(t, "late-group")
	if err := writer.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := writer.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.Cached(spec.RuntimeGroupID); ok {
		t.Fatal("cache-only lookup discovered a record created after startup")
	}
	if cache.Contains(spec.RuntimeGroupID) || cache.Contains(spec.SandboxID) {
		t.Fatal("cache-only identity check discovered a record created after startup")
	}
	if loaded, _, err := cache.Load(spec.RuntimeGroupID); err != nil || loaded.SandboxID != spec.SandboxID {
		t.Fatalf("explicit recovery load=%#v err=%v", loaded, err)
	}
	if !cache.Contains(spec.RuntimeGroupID) || !cache.Contains(spec.SandboxID) {
		t.Fatal("explicit load did not populate both identity indexes")
	}
}

func TestHeartbeatDeadlineIndexReturnsOnlyBoundedStaleGroups(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "groups"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id       string
		state    model.SandboxState
		observed time.Time
	}{
		{id: "group-a", state: model.StateReady, observed: time.Unix(10, 0).UTC()},
		{id: "group-b", state: model.StateActive, observed: time.Unix(20, 0).UTC()},
		{id: "group-terminal", state: model.StateStopped, observed: time.Unix(5, 0).UTC()},
	} {
		spec, status := testRecord(t, item.id)
		status.DesiredState = item.state
		status.ObservedState = item.state
		status.LastHeartbeat = item.observed
		if err := store.SaveSpec(spec); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveStatus(item.id, status); err != nil {
			t.Fatal(err)
		}
	}
	if due := store.ClaimStaleHeartbeats(time.Unix(15, 0).UTC(), 1); len(due) != 1 || due[0] != "group-a" {
		t.Fatalf("first due groups = %#v", due)
	}
	spec, _, err := store.Load("group-a")
	if err != nil {
		t.Fatal(err)
	}
	newest := time.Unix(30, 0).UTC()
	if applied, err := store.Observe("group-a", model.RuntimeSnapshot{
		Revision: 1, SupervisorStartedAtMS: 1, RuntimeGroupID: "group-a",
		SandboxID: spec.SandboxID, WorkloadType: spec.WorkloadType,
	}, newest); err != nil || !applied {
		t.Fatalf("new heartbeat applied=%t err=%v", applied, err)
	}
	store.RescheduleHeartbeat("group-a")
	if due := store.ClaimStaleHeartbeats(time.Unix(25, 0).UTC(), 10); len(due) != 1 || due[0] != "group-b" {
		t.Fatalf("second due groups = %#v", due)
	}
	if due := store.ClaimStaleHeartbeats(time.Unix(35, 0).UTC(), 10); len(due) != 1 || due[0] != "group-a" {
		t.Fatalf("refreshed due groups = %#v", due)
	}
}

func TestSupervisorSnapshotsAreAbsoluteRevisionedAndMemoryOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "groups")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	spec, status := testRecord(t, "group-a")
	status.ObservedState = model.StateReady
	if err := store.SaveSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(spec.RuntimeGroupID, status); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, spec.RuntimeGroupID, "state.json")
	durableBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Unix(10, 0).UTC()
	first := model.RuntimeSnapshot{
		Revision: 2, SupervisorStartedAtMS: 100, RuntimeGroupID: spec.RuntimeGroupID,
		SandboxID: spec.SandboxID, WorkloadType: spec.WorkloadType, WorkerCount: 1,
		Workers: []model.RuntimeWorkerStatus{{WorkerID: "worker-new", State: "ready"}},
	}
	if applied, err := store.Observe(spec.RuntimeGroupID, first, firstTime); err != nil || !applied {
		t.Fatalf("first observation applied=%t err=%v", applied, err)
	}
	stale := first
	stale.Revision = 1
	stale.Workers = []model.RuntimeWorkerStatus{{WorkerID: "worker-stale", State: "ready"}}
	secondTime := firstTime.Add(time.Second)
	if applied, err := store.Observe(spec.RuntimeGroupID, stale, secondTime); err != nil || applied {
		t.Fatalf("stale observation applied=%t err=%v", applied, err)
	}
	snapshot, ok := store.Snapshot(spec.RuntimeGroupID)
	if !ok || snapshot.Revision != 2 || snapshot.Workers[0].WorkerID != "worker-new" {
		t.Fatalf("snapshot rolled back = %#v", snapshot)
	}
	_, observed, err := store.Resolve(spec.SandboxID)
	if err != nil || !observed.LastHeartbeat.Equal(secondTime) {
		t.Fatalf("sandbox index or heartbeat freshness failed: %#v, %v", observed, err)
	}
	restarted := stale
	restarted.SupervisorStartedAtMS = 200
	if applied, err := store.Observe(spec.RuntimeGroupID, restarted, secondTime.Add(time.Second)); err != nil || !applied {
		t.Fatalf("new supervisor epoch applied=%t err=%v", applied, err)
	}
	snapshot, _ = store.Snapshot(spec.RuntimeGroupID)
	if snapshot.Revision != 1 || snapshot.Workers[0].WorkerID != "worker-stale" {
		t.Fatalf("new supervisor epoch was ignored = %#v", snapshot)
	}
	restartedAt := secondTime.Add(time.Second)
	olderSupervisor := first
	olderSupervisor.Revision = 3
	if applied, err := store.Observe(spec.RuntimeGroupID, olderSupervisor, restartedAt.Add(time.Second)); err != nil || applied {
		t.Fatalf("older supervisor applied=%t err=%v", applied, err)
	}
	_, observed, err = store.Resolve(spec.SandboxID)
	if err != nil || !observed.LastHeartbeat.Equal(restartedAt) {
		t.Fatalf("older supervisor refreshed heartbeat: %#v, %v", observed, err)
	}
	durableAfter, err := os.ReadFile(statePath)
	if err != nil || string(durableAfter) != string(durableBefore) {
		t.Fatalf("live observation changed durable state: %v", err)
	}
	reloaded, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := reloaded.Resolve(spec.SandboxID)
	if err != nil || resolved.InternalToken != spec.InternalToken {
		t.Fatalf("startup cache did not preload identity/token: %#v, %v", resolved, err)
	}
}
