//go:build linux

package rootless

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/discovery"
	"the8020/kernel/execution"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/programs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

type capturingRunscRunner struct {
	output string
}

func (r capturingRunscRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	if rootlessCommand(arguments) != "run" {
		return command.CombinedOutput()
	}
	output, err := os.OpenFile(r.output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	command.Stdout, command.Stderr = output, output
	return nil, command.Run()
}

func TestRealRunscSupervisorUsesMountedKernelSocket(t *testing.T) {
	if os.Getenv("THE8020_RUNSC_E2E") != "1" {
		t.Skip("set THE8020_RUNSC_E2E=1 to run the real rootless gVisor test")
	}
	runscPath := os.Getenv("THE8020_RUNSC_PATH")
	rootFS := os.Getenv("THE8020_RUNTIME_ROOTFS")
	if !filepath.IsAbs(runscPath) || !filepath.IsAbs(rootFS) {
		t.Fatal("absolute THE8020_RUNSC_PATH and THE8020_RUNTIME_ROOTFS are required")
	}

	callbackRoot := t.TempDir()
	callbackPath := filepath.Join(callbackRoot, "kernel.sock")
	listener, err := net.Listen("unix", callbackPath)
	if err != nil {
		t.Fatal(err)
	}
	callbackServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/runtime/database/scope" {
			var envelope map[string]any
			if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			envelope["message_type"], envelope["payload"] = "database_result", map[string]any{}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(envelope)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = callbackServer.Serve(listener) }()
	t.Cleanup(func() {
		_ = callbackServer.Close()
		_ = listener.Close()
	})

	supervisorPort := freeTCPPort(t)
	inspectorPort := freeTCPPort(t)
	for inspectorPort == supervisorPort {
		inspectorPort = freeTCPPort(t)
	}
	runtimeRoot := t.TempDir()
	outputPath := filepath.Join(runtimeRoot, "workload.log")
	backend, err := New(Config{
		RunscPath: runscPath, RootFS: rootFS,
		StateRoot: filepath.Join(runtimeRoot, "sandboxes"), RuntimeRoot: filepath.Join(runtimeRoot, "runsc"),
		LogRoot: filepath.Join(runtimeRoot, "logs"), InstanceUUID: "rootless-e2e", KernelSocketPath: "/run/the8020/kernel.sock",
		SupervisorHeartbeatInterval: 100 * time.Millisecond, WorkerStopGrace: time.Second, StartTimeout: 15 * time.Second,
		Runner: capturingRunscRunner{output: outputPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	packageSource := commandPackages(t)
	mounts := []model.Mount{
		{Source: packageSource.root, Target: "/workspace/packages", ReadOnly: true, Purpose: "workspace-packages", Persistence: "shared"},
		{Source: callbackRoot, Target: "/run/the8020", ReadOnly: true, Purpose: "kernel-api", Persistence: "kernel"},
		{Target: "/tmp", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
		{Target: "/runtime-cache", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
	}
	profile := model.RuntimeProfile{
		WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DependencyMode: model.DependencyOnline,
		Permissions:    model.Permissions{ReadPaths: []string{"/opt/runtime", "/workspace/packages", "/tmp", "/runtime-cache"}, WritePaths: []string{"/tmp", "/runtime-cache"}}, Mounts: mounts,
		NetworkMode: "netstack", EgressAllowed: true, ResourceClass: "job:e2e",
	}
	profileHash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	sandbox := model.SandboxSpec{
		SandboxID: "sandbox-e2e", RuntimeGroupID: "group-e2e", WorkloadType: model.WorkloadJob, GroupKey: "job:e2e", OwnerIDs: []string{"e2e"},
		ImageDigest: profile.ImageDigest, RuntimeProfile: profile, ProfileHash: profileHash,
		ResourceLimits: model.ResourceLimits{PIDMaximum: 64, TmpfsMaximum: 64 << 20},
		Network:        model.NetworkConfiguration{Mode: "netstack", NetworkName: "rootless-host", SandboxIP: "127.0.0.1", SupervisorPort: supervisorPort, InspectorPort: inspectorPort, EgressEnabled: true},
		Mounts:         mounts, Permissions: profile.Permissions, DependencyMode: profile.DependencyMode,
		Lifecycle:     model.LifecyclePolicy{DestroyWhenIdle: true, StopGracePeriod: time.Second},
		InternalToken: strings.Repeat("a", 64),
	}
	if _, err := backend.Create(context.Background(), sandbox); err != nil {
		t.Fatalf("create sandbox: %v\n%s", err, readDiagnostic(outputPath))
	}
	t.Cleanup(func() {
		_ = backend.Kill(context.Background(), sandbox.SandboxID)
		_ = backend.Delete(context.Background(), sandbox.SandboxID)
	})

	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, requestErr := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/status", supervisorPort), nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", "Bearer "+sandbox.InternalToken)
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				t.Run("package command uses ordinary job and cross-package imports", func(t *testing.T) {
					t.Cleanup(func() {
						if t.Failed() {
							t.Log(readDiagnostic(outputPath))
						}
					})
					verifyCommandJob(t, sandbox, packageSource)
					verifyHookJob(t, sandbox, packageSource)
				})
				return
			}
			lastErr = fmt.Errorf("status %s", response.Status)
		} else {
			lastErr = requestErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("supervisor did not become ready: %v\n%s", lastErr, readDiagnostic(outputPath))
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func readDiagnostic(path string) string {
	value, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(value)
}

// This adapter connects the real job manager to the already-running test
// sandbox. Ensure checks that commands preserve the ordinary runtime profile.
type commandRuntime struct {
	client *supervisor.Client
	spec   model.SandboxSpec
}

func (r *commandRuntime) Ensure(_ context.Context, request coordinator.Request) (manager.Inspection, error) {
	hash, err := request.Profile.Hash()
	if err != nil || hash != r.spec.ProfileHash {
		return manager.Inspection{}, fmt.Errorf("command changed ordinary job profile: hash=%s error=%v", hash, err)
	}
	return manager.Inspection{Spec: r.spec}, nil
}

func (r *commandRuntime) Release(context.Context, string, string, string) error { return nil }

func (r *commandRuntime) Inspect(ctx context.Context, _ string) (manager.Inspection, error) {
	live, err := r.client.Workers(ctx, r.spec)
	return manager.Inspection{
		Spec: r.spec, Workers: live,
		Status:  model.SandboxStatus{ObservedState: model.StateReady, SupervisorHealthy: true},
		Runtime: model.RuntimeSnapshot{ObservedAt: time.Now()},
	}, err
}

func (r *commandRuntime) List() ([]manager.Inspection, error) {
	inspection, err := r.Inspect(context.Background(), r.spec.RuntimeGroupID)
	return []manager.Inspection{inspection}, err
}

func (r *commandRuntime) ResolveRuntimeGroup(string) (model.SandboxSpec, error) {
	return r.spec, nil
}

type commandPackageSource struct {
	root string
}

func (p commandPackageSource) PackagesRoot() string { return p.root }
func (p commandPackageSource) ListPackageIndexes() ([]workspacepackages.PackageIndex, error) {
	return []workspacepackages.PackageIndex{{PackageID: "acme/commands", State: "ready", ActiveCommit: "active"}}, nil
}
func (p commandPackageSource) InspectPackageIndex(id string) (workspacepackages.PackageIndex, error) {
	if id != "acme/commands" {
		return workspacepackages.PackageIndex{}, os.ErrNotExist
	}
	return workspacepackages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "active"}, nil
}
func (p commandPackageSource) ActivatedPackageCommit(context.Context, string) (string, error) {
	return "active", nil
}
func (p commandPackageSource) ResolveProgram(_ context.Context, id string) (workspacepackages.ProgramDefinition, error) {
	identity, name, err := workspacepackages.ParseProgramID(id)
	if err != nil {
		return workspacepackages.ProgramDefinition{}, err
	}
	return workspacepackages.ValidateProgram(filepath.Join(p.root, identity.Namespace, identity.Repository), identity.PackageID(), name, "active")
}

func commandPackages(t *testing.T) commandPackageSource {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"acme/commands/package.toml":                 "schema = 1\ndescription = \"Command fixture\"\n",
		"acme/commands/cbus/commands/arbitrary.toml": "version = 1\ncommand = \"acme.commands.check\"\nprogram = \"check\"\nsummary = \"Check job execution\"\nrestart_behavior = \"none\"\n",
		"acme/commands/programs/check/program.toml":  "schema = 1\ndescription = \"Check job execution\"\n",
		"acme/commands/programs/check/program.ts": `
import { context } from "@the8020/context";
import { answer } from "/p/acme/dependency/mod.ts";
export default async (...args: unknown[]) => {
  const dynamic = await import("/p/acme/dependency/dynamic.ts");
  await Deno.writeTextFile("/tmp/command-check", "normal temp access");
  await Deno.writeTextFile("/runtime-cache/command-check", "normal cache access");
  const packages = [];
  for await (const entry of Deno.readDir("/workspace/packages")) packages.push(entry.name);
  return { answer: answer() + dynamic.default(), args, user: context.username, type: context.type, packages };
};
`,
		"acme/dependency/mod.ts":     "export const answer = () => 40;\n",
		"acme/dependency/dynamic.ts": "export default () => 2;\n",
	} {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return commandPackageSource{root: root}
}

func verifyCommandJob(t *testing.T, spec model.SandboxSpec, source commandPackageSource) {
	t.Helper()
	client, err := supervisor.New(supervisor.Config{ProtocolVersion: protocol.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &commandRuntime{client: client, spec: spec}
	workerManager, err := workers.New(runtime, client, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	jobManager, err := jobs.New(runtime, workerManager, jobs.Policy{
		Profile: spec.RuntimeProfile, ExecutionTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobManager.Close() })
	runner, err := programs.New(source, jobManager)
	if err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry(nil)
	indexer, err := discovery.New(source, runner, registry)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := indexer.Reindex(context.Background()); err != nil || report.Commands != 1 {
		t.Fatalf("command catalog=%#v error=%v", report, err)
	}
	user, _ := execution.UserForUsername("alice")
	ctx := execution.WithCaller(context.Background(), execution.Caller{User: user, ExecutionID: "parent", Workload: model.WorkloadService})
	response := registry.Execute(ctx, core.Request{
		ProtocolVersion: core.ProtocolVersion, CommandID: registry.Catalog().Commands[0].ID,
		Argv: []string{"two words", "--literal"},
	})
	if !response.Success {
		t.Fatalf("command failed: %#v", response.Error)
	}
	want := map[string]any{
		"answer": json.Number("42"), "args": []any{"two words", "--literal"},
		"user": "system", "type": "program", "packages": []any{"acme"},
	}
	if !reflect.DeepEqual(response.Result, want) {
		t.Fatalf("command result=%#v want=%#v", response.Result, want)
	}
	remaining, err := client.Workers(ctx, spec)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("job Worker cleanup: remaining=%#v error=%v", remaining, err)
	}
}

// The source index is a fixture; discovery, dispatcher admission, package
// mounts, supervisor, and Worker execution use their production implementations.
type hookPackageIndex struct {
	workspacepackages.PackageIndexStore
	entries  map[string]workspacepackages.PackageIndex
	revision uint64
}

func (s *hookPackageIndex) List(context.Context) ([]workspacepackages.PackageIndex, error) {
	result := []workspacepackages.PackageIndex{}
	for _, entry := range s.entries {
		result = append(result, entry)
	}
	return result, nil
}
func (s *hookPackageIndex) Get(_ context.Context, id string) (workspacepackages.PackageIndex, bool, error) {
	entry, ok := s.entries[id]
	return entry, ok, nil
}
func (s *hookPackageIndex) Revision(context.Context) (uint64, error) { return s.revision, nil }

func verifyHookJob(t *testing.T, spec model.SandboxSpec, source commandPackageSource) {
	t.Helper()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(source.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"acme/commands", "acme/dependency"} {
		write(id+"/package.toml", "schema = 1\ndescription = \"Hooks\"\n")
		write(id+"/programs/hook/program.toml", "schema = 1\ndescription = \"Hook\"\n")
	}
	write("acme/commands/hooks/first.toml", "hook = \"index-services\"\ndescription = \"Build\"\nprogram = \"acme/commands/hook\"\norder = 10\n")
	write("acme/dependency/hooks/second.toml", "hook = \"index-services\"\ndescription = \"Enhance\"\nprogram = \"acme/dependency/hook\"\norder = 20\n")
	write("acme/commands/programs/hook/program.ts", `
import { context } from "@the8020/context";
import { answer } from "/p/acme/dependency/mod.ts";
export default async (state, scope) => {
  if (!Object.isFrozen(scope) || scope.package_id !== "acme/owner") throw new Error("mutable or wrong scope");
  if (context.username !== "system") throw new Error("wrong principal");
  state.answer = answer() + (await import("/p/acme/dependency/dynamic.ts")).default();
  state.steps.push("build");
  state.worker = context.workerId;
  globalThis[Symbol.for("hook-state")] = state;
};
`)
	second := `
import { context } from "@the8020/context";
export default (state) => {
  if (globalThis[Symbol.for("hook-state")] !== state || context.workerId !== state.worker) throw new Error("state or Worker changed");
  state.steps.push("enhance");
  state.answer *= FACTOR;
};
`
	write("acme/dependency/programs/hook/program.ts", strings.ReplaceAll(second, "FACTOR", "1"))
	index := &hookPackageIndex{entries: map[string]workspacepackages.PackageIndex{}, revision: 1}
	for _, id := range []string{"acme/commands", "acme/dependency"} {
		index.entries[id] = workspacepackages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
	}
	store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: source.root, PackagesRoot: source.root, IndexStore: index})
	if err != nil {
		t.Fatal(err)
	}
	client, err := supervisor.New(supervisor.Config{ProtocolVersion: protocol.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &commandRuntime{client: client, spec: spec}
	workerManager, err := workers.New(runtime, client, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	jobManager, err := jobs.New(runtime, workerManager, jobs.Policy{Profile: spec.RuntimeProfile, ExecutionTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer jobManager.Close()
	run := func(expectFailure ...bool) jobs.Record {
		t.Helper()
		if _, err := store.ReindexHandlers(context.Background()); err != nil {
			t.Fatal(err)
		}
		record, err := store.RunHookChain(context.Background(), jobManager, "acme/owner", "index-services", store.Hooks("index-services"), map[string]any{"package_id": "acme/owner"}, map[string]any{"steps": []any{}}, nil)
		if len(expectFailure) == 0 && err != nil {
			t.Fatal(err)
		}
		if len(expectFailure) > 0 && (err == nil || !strings.Contains(err.Error(), "deliberate failure")) {
			t.Fatalf("expected propagated hook failure, got %v", err)
		}
		return record
	}
	first := run()
	check := func(record jobs.Record, answer string) {
		t.Helper()
		result, ok := record.Result.(map[string]any)
		if !ok || fmt.Sprint(result["answer"]) != answer || !reflect.DeepEqual(result["steps"], []any{"build", "enhance"}) || fmt.Sprint(result["worker"]) == "" {
			t.Fatalf("hook result: %#v", record)
		}
	}
	check(first, "42")
	check(run(), "42") // Ordinary sandbox reuse keeps normal mounts and permissions.
	write("acme/dependency/programs/hook/program.ts", strings.ReplaceAll(second, "FACTOR", "2"))
	entry := index.entries["acme/dependency"]
	entry.ActiveCommit = "second"
	index.entries[entry.PackageID] = entry
	index.revision++
	updated := run()
	check(updated, "84")
	if updated.ReleaseID == first.ReleaseID {
		t.Fatal("changed handler retained old release")
	}
	write("acme/dependency/programs/hook/program.ts", `export default () => { throw new Error("deliberate failure") };`)
	entry.ActiveCommit = "third"
	index.entries[entry.PackageID] = entry
	index.revision++
	failed := run(true)
	if failed.State != "FAILED" || !strings.Contains(failed.Failure, "acme/dependency/") || !strings.Contains(failed.Failure, "deliberate failure") {
		t.Fatalf("hook failure: %#v", failed)
	}
	remaining, err := client.Workers(context.Background(), spec)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("hook Worker cleanup: %#v %v", remaining, err)
	}
}
