package containerd

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"the8020/kernel/sandbox/model"
)

func TestNamespaceAndOwnershipAreInstanceScoped(t *testing.T) {
	if got := NamespaceForInstance("ABC/123"); got != "the8020-abc-123" {
		t.Fatalf("namespace = %q", got)
	}
	backend := &Backend{instanceUUID: "instance-one"}
	if !backend.owns(map[string]string{labelManaged: "true", labelInstance: "instance-one"}) {
		t.Fatal("matching labels not owned")
	}
	if backend.owns(map[string]string{labelManaged: "true", labelInstance: "instance-two"}) {
		t.Fatal("foreign instance labels owned")
	}
	sandbox := testSandbox(t)
	sandbox.Labels = map[string]string{labelManaged: "false"}
	if _, err := backend.labels(sandbox); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved-label error = %v", err)
	}
}

func TestMutableOwnerLabelsSupportSharedGroups(t *testing.T) {
	if err := validateLabelUpdates(map[string]string{labelOwner: "first", labelOwners: "first,second", labelServices: "the8020/demo/api,the8020/demo/api", labelGroupKey: "service:shared", labelAssignedAt: "2026-08-20T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := validateLabelUpdates(map[string]string{labelRuntimeGroup: "other"}); err == nil {
		t.Fatal("reserved runtime identity label was mutable")
	}
}

func TestSandboxOCIOptionEnforcesBoundaryAndLimits(t *testing.T) {
	sandbox := testSandbox(t)
	generated := &oci.Spec{
		Root:    &specs.Root{Path: "rootfs"},
		Process: &specs.Process{Args: []string{"deno", "run", "--cached-only", "--allow-read=/opt/runtime", "--allow-write=/tmp/runtime", "--allow-net=0.0.0.0:8000", "main.ts"}, Env: []string{"PATH=/bin"}, Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_SYS_ADMIN"}}},
		Linux:   &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.NetworkNamespace}}},
	}
	option := sandboxSpecOption(sandbox, "instance-one", "http://10.0.0.1:9123", 8000, 5*time.Second, 1500*time.Millisecond)
	if err := option(context.Background(), nil, &containers.Container{}, generated); err != nil {
		t.Fatal(err)
	}
	if !generated.Root.Readonly || !generated.Process.NoNewPrivileges || len(generated.Process.Capabilities.Bounding) != 0 {
		t.Fatalf("security boundary: %#v %#v", generated.Root, generated.Process)
	}
	if generated.Linux.Resources.Unified["memory.max"] != "268435456" || generated.Linux.Resources.Unified["pids.max"] != "64" || !strings.Contains(generated.Linux.CgroupsPath, sandbox.SandboxID) {
		t.Fatalf("resources: %#v cgroup %q", generated.Linux.Resources, generated.Linux.CgroupsPath)
	}
	if generated.Linux.Namespaces[0].Path != sandbox.Network.NamespacePath {
		t.Fatalf("network namespace: %#v", generated.Linux.Namespaces)
	}
	if len(generated.Mounts) != 3 || generated.Mounts[0].Options[1] != "ro" || !boundedTmpfs(generated.Mounts, "/tmp/runtime", 16777216) || !boundedTmpfs(generated.Mounts, "/runtime-cache", 16777216) {
		t.Fatalf("mounts: %#v", generated.Mounts)
	}
	environment := strings.Join(generated.Process.Env, "\n")
	for _, expected := range []string{"INTERNAL_API_TOKEN=secret", "RUNTIME_GROUP_ID=group-one", "WORKLOAD_TYPE=job", "HEARTBEAT_INTERVAL_MS=5000", "WORKER_STOP_GRACE_MS=1500"} {
		if !strings.Contains(environment, expected) {
			t.Errorf("environment missing %q: %s", expected, environment)
		}
	}
	arguments := strings.Join(generated.Process.Args, " ")
	for _, expected := range []string{"--allow-read=", "/workspace/user-one", "--allow-write=", "/runtime-cache", "--allow-net=", "10.0.0.1:9123"} {
		if !strings.Contains(arguments, expected) {
			t.Errorf("arguments missing %q: %s", expected, arguments)
		}
	}
}

func TestOnlineDependencyModeRemovesCachedOnly(t *testing.T) {
	sandbox := testSandbox(t)
	sandbox.DependencyMode = model.DependencyOnline
	sandbox.RuntimeProfile.DependencyMode = model.DependencyOnline
	sandbox.RuntimeProfile.EgressAllowed = true
	sandbox.Permissions.ImportHosts = []string{"deno.land"}
	sandbox.RuntimeProfile.Permissions.ImportHosts = []string{"deno.land"}
	arguments := parentPermissionArgs([]string{"deno", "run", "--cached-only", "main.ts"}, sandbox, "", 8000)
	if containsArgument(arguments, "--cached-only") {
		t.Fatalf("cached-only remains: %#v", arguments)
	}
	if !containsArgument(arguments, "--allow-import=deno.land") {
		t.Fatalf("import permission missing: %#v", arguments)
	}
}

func testSandbox(t *testing.T) model.SandboxSpec {
	t.Helper()
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profileMounts := []model.Mount{{Source: "/data/artifacts", Target: "/artifacts/app", ReadOnly: true, Purpose: "artifact", Persistence: "persistent"}, {Target: "/tmp/runtime", MaximumSize: 16777216, Purpose: "temporary", Persistence: "ephemeral"}, {Target: "/runtime-cache", MaximumSize: 16777216, Purpose: "temporary", Persistence: "ephemeral"}}
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: digest, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/workspace/user-one"}}, Mounts: profileMounts, NetworkMode: "netstack", ResourceClass: "default"}
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return model.SandboxSpec{
		SandboxID: "sandbox-one", RuntimeGroupID: "group-one", WorkloadType: model.WorkloadJob,
		GroupKey: "user-one", OwnerIDs: []string{"user-one"}, ImageDigest: digest, RuntimeProfile: profile, ProfileHash: hash,
		ResourceLimits: model.ResourceLimits{MemoryHigh: 134217728, MemoryMaximum: 268435456, SwapMaximum: 0, CPUQuotaMicros: 100000, CPUPeriodMicros: 100000, CPUWeight: 100, PIDMaximum: 64, TmpfsMaximum: 16777216},
		Network:        model.NetworkConfiguration{Mode: "netstack", NamespacePath: "/var/run/netns/the8020-one", NetworkName: "the8020"},
		Mounts:         append([]model.Mount(nil), profileMounts...),
		Permissions:    model.Permissions{ReadPaths: []string{"/workspace/user-one"}}, DependencyMode: model.DependencyCachedOnly,
		Lifecycle: model.LifecyclePolicy{}, InternalToken: "secret",
	}
}

func containsArgument(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func boundedTmpfs(values []specs.Mount, target string, size int64) bool {
	for _, value := range values {
		if value.Destination == target && value.Type == "tmpfs" && containsArgument(value.Options, "size="+strconv.FormatInt(size, 10)) && containsArgument(value.Options, "mode=1777") {
			return true
		}
	}
	return false
}
