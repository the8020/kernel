package profile

import (
	"os"
	"path/filepath"
	"testing"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/mounts"
)

func TestForWorkerDerivesCompatibilityEnvelope(t *testing.T) {
	base := testProfile()
	derived, err := ForWorker(base, &supervisor.WorkerPermissions{Read: []string{"/artifacts/app"}, Write: []string{"/tmp/work"}, Net: []string{"192.0.2.4:443"}, Import: []string{"deno.land"}, Env: []string{"APP_MODE"}, Sys: []string{"hostname"}})
	if err != nil {
		t.Fatal(err)
	}
	if derived.DependencyMode != model.DependencyOnline || len(derived.Permissions.NetworkHosts) != 1 || len(derived.Permissions.ImportHosts) != 1 || !derived.Permissions.SystemInfo {
		t.Fatalf("derived=%#v", derived)
	}
	baseHash, _ := base.Hash()
	derivedHash, _ := derived.Hash()
	if baseHash == derivedHash {
		t.Fatal("permission-derived profile did not split compatibility")
	}
}

func TestForWorkerRejectsFilesystemHostAndEnvironmentEscapes(t *testing.T) {
	base := testProfile()
	for _, requested := range []*supervisor.WorkerPermissions{
		{Read: []string{"/etc"}},
		{Write: []string{"relative"}},
		{Net: []string{"https://example.com/path"}},
		{Env: []string{"INTERNAL_API_TOKEN"}},
	} {
		if _, err := ForWorker(base, requested); err == nil {
			t.Fatalf("accepted %#v", requested)
		}
	}
}

func TestForWorkerRejectsEgressWhenGlobalProfilePolicyDisablesIt(t *testing.T) {
	base := testProfile()
	base.EgressAllowed = false
	if _, err := ForWorker(base, &supervisor.WorkerPermissions{Net: []string{"example.com:443"}}); err == nil {
		t.Fatal("network permission bypassed disabled sandbox egress policy")
	}
}

func TestForWorkerWithWorkspaceAddsPolicyApprovedCompatibleMount(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "development")
	state := filepath.Join(root, "node", "kernel")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := mounts.NewPolicy([]string{root}, state, "/run/containerd/containerd.sock", true)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := ForWorkerWithWorkspace(testProfile(), &supervisor.WorkerPermissions{Read: []string{"/workspace"}, Write: []string{"/workspace/output"}}, Workspace{Source: "development", OwnerID: "owner", Writable: true}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(derived.Mounts) != 1 || derived.Mounts[0].Source != workspace || derived.Mounts[0].Target != "/workspace" || derived.Mounts[0].ReadOnly || derived.Mounts[0].OwnerScope != "owner" {
		t.Fatalf("derived=%#v", derived)
	}
	if _, err := ForWorkerWithWorkspace(testProfile(), &supervisor.WorkerPermissions{Write: []string{"/workspace"}}, Workspace{Source: "development", OwnerID: "owner"}, policy); err == nil {
		t.Fatal("read-only workspace granted write permission")
	}
	if _, err := ForWorkerWithWorkspace(testProfile(), nil, Workspace{OwnerID: "owner", Writable: true}, policy); err == nil {
		t.Fatal("workspace write flag without a source was accepted")
	}
	firstHash, _ := derived.Hash()
	otherOwner, err := ForWorkerWithWorkspace(testProfile(), nil, Workspace{Source: "development", OwnerID: "other", Writable: true}, policy)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, _ := otherOwner.Hash()
	if firstHash == otherHash {
		t.Fatal("owner-scoped workspace did not split runtime compatibility")
	}
}

func testProfile() model.RuntimeProfile {
	return model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/artifacts"}, WritePaths: []string{"/tmp"}}, NetworkMode: "netstack", EgressAllowed: true, ResourceClass: "job"}
}
