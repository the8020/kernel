package adminrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/model"
)

type fakeJobs struct {
	jobID, entrypoint string
	sandboxID         string
	options           jobs.Options
	calls             int
}

type fakeMetrics struct {
	sandboxID string
	value     model.ResourceMetrics
}

func (f *fakeMetrics) Metrics(sandboxID string) (model.ResourceMetrics, error) {
	f.sandboxID = sandboxID
	return f.value, nil
}

func (f *fakeJobs) Run(_ context.Context, jobID, entrypoint string, options jobs.Options) (jobs.Record, error) {
	f.jobID, f.entrypoint, f.options, f.calls = jobID, entrypoint, options, f.calls+1
	return jobs.Record{ExecutionID: "execution-test", JobID: jobID, Entrypoint: entrypoint, SandboxID: f.sandboxID, State: "SUCCEEDED"}, nil
}

func TestEvalMaterializesWrapperAndOnlyDelegatesToJobs(t *testing.T) {
	root, artifacts := t.TempDir(), filepath.Join(t.TempDir(), "artifacts")
	fake := &fakeJobs{sandboxID: "sandbox-test"}
	manager, err := New(Config{InstanceRoot: root, ArtifactsRoot: artifacts, Jobs: fake})
	if err != nil {
		t.Fatal(err)
	}
	permissions := &supervisor.WorkerPermissions{Read: []string{"/artifacts"}}
	result, err := manager.Eval(context.Background(), "export default 1 + 1", Options{OwnerID: "admin", Detached: true, Input: map[string]any{"a": 1}, Permissions: permissions, Workspace: "development", WorkspaceWritable: true})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 || fake.jobID != "runtime-eval" || result.Execution.ExecutionID != "execution-test" || !strings.HasPrefix(fake.entrypoint, "file:///artifacts/") {
		t.Fatalf("delegation=%#v result=%#v", fake, result)
	}
	if fake.options.OwnerID != "admin" || fake.options.Permissions == permissions || len(fake.options.Permissions.Read) != 1 || fake.options.Permissions.Read[0] != "/artifacts" || fake.options.Workspace != "development" || !fake.options.WorkspaceWritable {
		t.Fatalf("options=%#v", fake.options)
	}
	directory := filepath.Join(artifacts, result.ArtifactID)
	module, err := os.ReadFile(filepath.Join(directory, "module.ts"))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile(filepath.Join(directory, "entry.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), "export default 1 + 1") || !strings.Contains(string(wrapper), `import value from "./module.ts"`) {
		t.Fatalf("module=%s wrapper=%s", module, wrapper)
	}
	directoryInfo, directoryErr := os.Stat(directory)
	moduleInfo, moduleErr := os.Stat(filepath.Join(directory, "module.ts"))
	if directoryErr != nil || moduleErr != nil {
		t.Fatalf("inspect sandbox-readable artifact modes: %v/%v", directoryErr, moduleErr)
	}
	if directoryInfo.Mode().Perm() != 0o755 || moduleInfo.Mode().Perm() != 0o644 {
		t.Fatalf("sandbox-readable artifact modes directory=%v module=%v", directoryInfo.Mode().Perm(), moduleInfo.Mode().Perm())
	}
}

func TestAdministrativeExecutionRejectsNonJobWorkloadType(t *testing.T) {
	manager, err := New(Config{InstanceRoot: t.TempDir(), ArtifactsRoot: filepath.Join(t.TempDir(), "artifacts"), Jobs: &fakeJobs{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Eval(context.Background(), "export default 1", Options{WorkloadType: "service"}); err == nil || !strings.Contains(err.Error(), "job") {
		t.Fatalf("error=%v", err)
	}
}

func TestAdministrativeExecutionIncludesSynchronousResourceSnapshot(t *testing.T) {
	root, artifacts := t.TempDir(), filepath.Join(t.TempDir(), "artifacts")
	fake := &fakeJobs{sandboxID: "sandbox-test"}
	metrics := &fakeMetrics{value: model.ResourceMetrics{CPUUsageMicros: 42, MemoryPeak: 2048}}
	manager, err := New(Config{InstanceRoot: root, ArtifactsRoot: artifacts, Jobs: fake, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Eval(context.Background(), "export default 42", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.sandboxID != "sandbox-test" || result.Execution.SandboxID != "sandbox-test" || result.Resources == nil || result.Resources.CPUUsageMicros != 42 || result.Resources.MemoryPeak != 2048 {
		t.Fatalf("metrics sandbox=%q execution=%#v resources=%#v", metrics.sandboxID, result.Execution, result.Resources)
	}
	if fake.options.Permissions == nil || len(fake.options.Permissions.Read) != 1 || fake.options.Permissions.Read[0] != "/artifacts/"+result.ArtifactID {
		t.Fatalf("default artifact permissions=%#v", fake.options.Permissions)
	}
}

func TestRunCopiesLocalImportsAndRejectsUnsafeSources(t *testing.T) {
	root, artifacts := t.TempDir(), filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(filepath.Join(root, "program"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "program", "dep.ts"), []byte("export const value = 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "program", "main.ts"), []byte(`import { value } from "./dep.ts"; export default value;`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeJobs{}
	manager, err := New(Config{InstanceRoot: root, ArtifactsRoot: artifacts, Jobs: fake, MaximumFiles: 2, MaximumBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "program/main.ts", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(artifacts, result.ArtifactID, "dep.ts")); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Stat(filepath.Join(artifacts, result.ArtifactID, "dep.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if copied.Mode().Perm() != 0o644 {
		t.Fatalf("copied artifact mode=%v", copied.Mode().Perm())
	}
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, []byte("export default 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), outside, Options{}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside error=%v", err)
	}
	symlink := filepath.Join(root, "program", "link.ts")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "program/main.ts", Options{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error=%v", err)
	}
}

func TestRunEnforcesArtifactLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.ts"), []byte("export default 123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{InstanceRoot: root, ArtifactsRoot: filepath.Join(t.TempDir(), "artifacts"), Jobs: &fakeJobs{}, MaximumFiles: 1, MaximumBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "main.ts", Options{}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit error=%v", err)
	}
}
