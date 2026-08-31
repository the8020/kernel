package mounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/sandbox/model"
)

func TestMountPolicyAllowsControlledArtifactsWorkspacesAndTmpfs(t *testing.T) {
	root := t.TempDir()
	artifacts := filepath.Join(root, "artifacts")
	workspace := filepath.Join(root, "workspace")
	state := filepath.Join(root, "node", "kernel")
	for _, path := range []string{artifacts, workspace, state} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := NewPolicy([]string{artifacts, workspace}, state, "/run/containerd/containerd.sock", false)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := policy.Validate(model.Mount{Source: artifacts, Target: "/artifacts/program", ReadOnly: true, Purpose: "artifact", Persistence: "execution"})
	if err != nil || artifact.Source != artifacts {
		t.Fatalf("artifact = %#v, error = %v", artifact, err)
	}
	if _, err := policy.Validate(model.Mount{Source: workspace, Target: "/workspace/dev", Purpose: "workspace", Persistence: "runtime_group"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if _, err := policy.Validate(model.Mount{Target: "/tmp/execution", MaximumSize: 1024, Purpose: "temporary", Persistence: "execution"}); err != nil {
		t.Fatalf("tmpfs: %v", err)
	}
}

func TestMountPolicyRejectsEscapesAndProtectedPaths(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	state := filepath.Join(root, "node", "kernel")
	for _, path := range []string{allowed, outside, state} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	symlink := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy([]string{allowed}, state, "/run/containerd/containerd.sock", false)
	if err != nil {
		t.Fatal(err)
	}
	tests := []model.Mount{
		{Source: outside, Target: "/workspace/out", Purpose: "workspace", Persistence: "execution"},
		{Source: symlink, Target: "/workspace/link", Purpose: "workspace", Persistence: "execution"},
		{Source: allowed, Target: "/opt/runtime", ReadOnly: true, Purpose: "artifact", Persistence: "execution"},
		{Source: allowed, Target: "/artifacts/code", Purpose: "artifact", Persistence: "execution"},
		{Source: state, Target: "/workspace/state", Purpose: "workspace", Persistence: "execution"},
	}
	for index, request := range tests {
		if _, err := policy.Validate(request); err == nil {
			t.Errorf("request %d was accepted: %#v", index, request)
		}
	}
}

func TestGroupedMountRequiresOwnerScope(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy([]string{allowed}, filepath.Join(root, "node", "kernel"), "/run/containerd/containerd.sock", true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = policy.Validate(model.Mount{Source: allowed, Target: "/workspace/user", Purpose: "workspace", Persistence: "runtime_group"})
	if err == nil || !strings.Contains(err.Error(), "owner scope") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkspaceSourceIsProjectRelativeAndCannotContainKernelState(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "development")
	state := filepath.Join(root, "node", "kernel")
	for _, path := range []string{workspace, state} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := NewPolicy([]string{root}, state, "/run/containerd/containerd.sock", true)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := policy.Validate(model.Mount{Source: "development", Target: "/workspace", ReadOnly: true, OwnerScope: "owner", Purpose: "workspace", Persistence: "development"})
	if err != nil || mount.Source != workspace {
		t.Fatalf("mount=%#v err=%v", mount, err)
	}
	if _, err := policy.Validate(model.Mount{Source: root, Target: "/workspace", ReadOnly: true, OwnerScope: "owner", Purpose: "workspace", Persistence: "development"}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("instance root containing kernel state accepted: %v", err)
	}
}
