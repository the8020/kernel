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
