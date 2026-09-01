package rootless

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/sandbox/model"
)

type fakeRunner struct {
	mu      sync.Mutex
	status  string
	lastRun []string
}

func (r *fakeRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastRun = append([]string(nil), arguments...)
	command := commandArgument(arguments)
	switch command {
	case "run":
		r.status = "running"
		return nil, nil
	case "state":
		if r.status == "" {
			return nil, errors.New("container not found")
		}
		return json.Marshal(map[string]any{"id": "sandbox-one", "status": r.status, "pid": 42, "bundle": "/ignored"})
	case "kill":
		r.status = "stopped"
		return nil, nil
	case "delete":
		r.status = ""
		return nil, nil
	case "events":
		return []byte(`{"type":"stats","data":{"cpu":{"usage":{"total":12000}},"memory":{"usage":{"usage":4096,"max":8192}},"pids":{"current":3}}}`), nil
	default:
		return nil, errors.New("unexpected command")
	}
}

func commandArgument(arguments []string) string {
	for _, argument := range arguments {
		switch argument {
		case "run", "state", "kill", "delete", "events":
			return argument
		}
	}
	return ""
}

func TestMutableOwnerLabelsSupportSharedServiceGroups(t *testing.T) {
	if err := validateLabelUpdates(map[string]string{
		labelOwner: "first", labelOwners: "first,second",
		labelServices: "the8020/demo/api,the8020/demo/api",
		labelGroupKey: "service:shared", labelAssignedAt: "2026-08-20T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateLabelUpdates(map[string]string{labelRuntimeGroup: "other"}); err == nil {
		t.Fatal("reserved runtime identity label was mutable")
	}
}

func TestRootlessBackendBuildsRestrictedOCIAndRunsLifecycle(t *testing.T) {
	backend := testBackend(t)
	sandbox := testSandbox(t)
	configuration, err := backend.ociSpec(sandbox, filepath.Join(backend.sandboxPath(sandbox.SandboxID), "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Root == nil || configuration.Root.Path != backend.rootFS || configuration.Root.Readonly || configuration.Process == nil || !configuration.Process.NoNewPrivileges {
		t.Fatalf("root/process=%#v %#v", configuration.Root, configuration.Process)
	}
	if configuration.Process.User.UID != 0 || configuration.Process.Capabilities == nil || len(configuration.Process.Capabilities.Bounding) != 0 {
		t.Fatalf("rootless process identity/capabilities=%#v", configuration.Process)
	}
	for _, namespace := range configuration.Linux.Namespaces {
		if namespace.Type == "network" {
			t.Fatal("rootless host-network OCI spec unexpectedly created a network namespace")
		}
	}
	arguments := strings.Join(configuration.Process.Args, " ")
	if !strings.Contains(arguments, "--inspect=127.0.0.1:19229") || !strings.Contains(arguments, "--allow-net=127.0.0.1:18000") {
		t.Fatalf("arguments=%s", arguments)
	}
	environment := strings.Join(configuration.Process.Env, " ")
	if !strings.Contains(environment, "SUPERVISOR_HOST=127.0.0.1") || !strings.Contains(environment, "SUPERVISOR_PORT=18000") {
		t.Fatalf("environment=%s", environment)
	}

	observation, err := backend.Create(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Runtime != "runsc-rootless-systrap" || observation.TaskStatus != "running" || observation.TaskPID != 42 {
		t.Fatalf("observation=%#v", observation)
	}
	metrics, err := backend.Metrics(context.Background(), sandbox.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.CPUUsageMicros != 12 || metrics.MemoryCurrent != 4096 || metrics.MemoryPeak != 8192 || metrics.PIDCurrent != 3 {
		t.Fatalf("metrics=%#v", metrics)
	}
	if err := backend.Stop(context.Background(), sandbox.SandboxID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(context.Background(), sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backend.sandboxPath(sandbox.SandboxID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox state survived delete: %v", err)
	}
}

func TestListOwnedReadsMetadataWithoutRunscStateProbe(t *testing.T) {
	backend := testBackend(t)
	sandbox := testSandbox(t)
	if _, err := backend.Create(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	runner := backend.runner.(*fakeRunner)
	runner.mu.Lock()
	runner.lastRun = nil
	runner.mu.Unlock()
	owned, err := backend.ListOwned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].ContainerID != sandbox.SandboxID || owned[0].RuntimeGroupID != sandbox.RuntimeGroupID || owned[0].TaskStatus != "" {
		t.Fatalf("owned=%#v", owned)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.lastRun) != 0 {
		t.Fatalf("ListOwned invoked runsc: %#v", runner.lastRun)
	}
}

func TestSandboxPIDsOnlyInspectsCurrentSubreaperChildren(t *testing.T) {
	backend := testBackend(t)
	backend.subreaper = true
	backend.procRoot = t.TempDir()
	taskRoot := filepath.Join(backend.procRoot, strconv.Itoa(os.Getpid()), "task")
	for task, children := range map[string]string{
		"101": "200 201\n",
		"102": "201 202\n",
	} {
		path := filepath.Join(taskRoot, task)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "children"), []byte(children), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commands := map[string]string{
		"200": "runsc-sandbox\x00--root=/runtime\x00sandbox-target\x00",
		"201": "runsc-gofer\x00--bundle=/state/sandbox-target/bundle\x00",
		"202": "runsc-sandbox\x00--root=/runtime\x00sandbox-other\x00",
		"999": "runsc-gofer\x00--bundle=/state/sandbox-target/bundle\x00",
	}
	for pid, command := range commands {
		path := filepath.Join(backend.procRoot, pid)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cmdline"), []byte(command), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		"200/task/200/schedstat": "1000000 0 0\n",
		"200/task/203/schedstat": "2000000 0 0\n",
		"201/task/201/schedstat": "4000000 0 0\n",
	} {
		path = filepath.Join(backend.procRoot, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := backend.sandboxPIDs("sandbox-target"), []int{200, 201}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox PIDs=%v want=%v", got, want)
	}
	if usage, ok := backend.processCPUUsageMicros("sandbox-target"); !ok || usage != 7000 {
		t.Fatalf("sandbox process CPU usage=%d available=%t", usage, ok)
	}
}

func testBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	runsc := filepath.Join(root, "runsc")
	if err := os.WriteFile(runsc, []byte("runsc"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootFS := filepath.Join(root, "rootfs")
	if err := os.Mkdir(rootFS, 0o700); err != nil {
		t.Fatal(err)
	}
	value, err := New(Config{
		RunscPath: runsc, RootFS: rootFS, StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"),
		LogRoot: filepath.Join(root, "logs"), InstanceUUID: "instance-one", CallbackAddress: "http://127.0.0.1:19000",
		SupervisorHeartbeatInterval: time.Second, WorkerStopGrace: time.Second, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testSandbox(t *testing.T) model.SandboxSpec {
	t.Helper()
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	limits := model.ResourceLimits{PIDMaximum: 1, TmpfsMaximum: 1}
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: digest, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{}, NetworkMode: "netstack", ResourceClass: "job:test"}
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return model.SandboxSpec{
		SandboxID: "sandbox-one", RuntimeGroupID: "group-one", WorkloadType: model.WorkloadJob, GroupKey: "job:one", OwnerIDs: []string{"one"},
		ImageDigest: digest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: limits,
		Network:     model.NetworkConfiguration{Mode: "netstack", NetworkName: "rootless-host", SandboxIP: "127.0.0.1", SupervisorPort: 18000, InspectorPort: 19229},
		Permissions: profile.Permissions, DependencyMode: profile.DependencyMode, Lifecycle: model.LifecyclePolicy{StopGracePeriod: time.Second}, InternalToken: "0123456789abcdef0123456789abcdef",
	}
}
