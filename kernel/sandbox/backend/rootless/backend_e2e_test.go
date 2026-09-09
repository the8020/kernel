//go:build linux

package rootless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
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
	"the8020/kernel/identity"
	"the8020/kernel/logging"
	"the8020/kernel/logging/records"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/ports"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/history"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	sandboxnetwork "the8020/kernel/sandbox/network"
	"the8020/kernel/sandbox/state"
)

func TestRealRunscSupervisorUsesMountedKernelSocket(t *testing.T) {
	if os.Getenv("THE8020_RUNSC_E2E") != "1" {
		t.Skip("set THE8020_RUNSC_E2E=1 to run the real rootless gVisor test")
	}
	runscPath := os.Getenv("THE8020_RUNSC_PATH")
	rootFS := os.Getenv("THE8020_RUNTIME_ROOTFS")
	if !filepath.IsAbs(runscPath) || !filepath.IsAbs(rootFS) {
		t.Fatal("absolute THE8020_RUNSC_PATH and THE8020_RUNTIME_ROOTFS are required")
	}

	runtimeRoot, err := os.MkdirTemp("", "8020-runsc-logs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	callbackRoot := filepath.Join(runtimeRoot, "api")
	if err := os.Mkdir(callbackRoot, 0700); err != nil {
		t.Fatal(err)
	}
	logd := filepath.Join(runtimeRoot, "logd")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", logd, "../../../logd")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	logManager, err := logging.New(logging.Config{
		Directory: filepath.Join(runtimeRoot, "logs"), Socket: filepath.Join(callbackRoot, "logs.sock"),
		Executable: logd, NodeID: "nod-0123456789", MaxProducers: 4,
		Policy: records.Policy{Enabled: true, Level: "info", SplitBy: "none", SplitPeriod: "day", MaxFileSize: 1 << 20, MaxTotalSize: 8 << 20, MaxAge: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logManager.Close() })
	for deadline := time.Now().Add(5 * time.Second); !logManager.Status().Available; {
		if time.Now().After(deadline) {
			t.Fatalf("logd unavailable: %#v", logManager.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	callbackPath := filepath.Join(callbackRoot, "kernel.sock")
	listener, err := net.Listen("unix", callbackPath)
	if err != nil {
		t.Fatal(err)
	}
	callbackServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var envelope map[string]any
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if request.URL.Path == "/v1/runtime/database/scope" {
			envelope["message_type"], envelope["payload"] = "database_result", map[string]any{}
		}
		// Production callbacks acknowledge with a JSON envelope. Keep that same
		// framing here so registration and heartbeat completion are exercised.
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(envelope)
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
	backend, err := New(Config{
		RunscPath: runscPath, RootFS: rootFS,
		StateRoot: filepath.Join(runtimeRoot, "sandboxes"), RuntimeRoot: filepath.Join(runtimeRoot, "runsc"),
		InstanceUUID: "nod-0123456789", KernelSocketPath: "/run/the8020/kernel.sock",
		SupervisorHeartbeatInterval: 100 * time.Millisecond, WorkerStopGrace: time.Second, StartTimeout: 15 * time.Second,
		Logger: logManager.Logger(),
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
	for _, part := range []string{"npm", "remote", "gen"} {
		source := filepath.Join(runtimeRoot, "cache", part)
		if err := os.MkdirAll(source, 0755); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, model.Mount{Source: source, Target: "/runtime-cache/" + part, Purpose: "runtime-cache", Persistence: "node"})
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
		SandboxID: "sbx-0123456789", WorkloadType: model.WorkloadJob, GroupKey: "job:e2e", OwnerIDs: []string{"e2e"},
		ImageDigest: profile.ImageDigest, RuntimeProfile: profile, ProfileHash: profileHash,
		ResourceLimits: model.ResourceLimits{PIDMaximum: 64, TmpfsMaximum: 64 << 20},
		Network:        model.NetworkConfiguration{Mode: "netstack", NetworkName: "rootless-host", SandboxIP: "127.0.0.1", SupervisorPort: supervisorPort, InspectorPort: inspectorPort, EgressEnabled: true},
		Mounts:         mounts, Permissions: profile.Permissions, DependencyMode: profile.DependencyMode,
		Lifecycle:     model.LifecyclePolicy{DestroyWhenIdle: true, StopGracePeriod: time.Second},
		InternalToken: strings.Repeat("a", 64),
	}
	output, err := logManager.RegisterSandbox(context.Background(), sandbox.SandboxID, sandbox.InternalToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Create(context.Background(), sandbox, output); err != nil {
		t.Fatalf("create sandbox: %v\n%s", err, readDiagnostic(logManager))
	}
	t.Cleanup(func() {
		_ = backend.Kill(context.Background(), sandbox.SandboxID)
		_ = backend.Delete(context.Background(), sandbox.SandboxID)
		_ = logManager.UnregisterSandbox(context.Background(), sandbox.SandboxID)
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
							t.Log(readDiagnostic(logManager))
						}
					})
					t.Run("commands", func(t *testing.T) { verifyCommandJob(t, sandbox, packageSource, logManager) })
					t.Run("hooks", func(t *testing.T) { verifyHookJob(t, sandbox, packageSource, logManager) })
					t.Run("logs", func(t *testing.T) { verifyPersistedRuntimeLogs(t, logManager, sandbox.SandboxID) })
				})
				t.Run("sandbox history keeps log references through assignment recovery and cleanup", func(t *testing.T) {
					verifySandboxHistoryLifecycle(t, runscPath, rootFS, filepath.Join(runtimeRoot, "history-check"), sandbox, logManager)
				})
				t.Run("concurrent service Workers preserve users parents and persistent bindings", func(t *testing.T) {
					verifyConcurrentServiceLogs(t, backend, sandbox, logManager)
				})
				return
			}
			lastErr = fmt.Errorf("status %s", response.Status)
		} else {
			lastErr = requestErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("supervisor did not become ready: %v\n%s", lastErr, readDiagnostic(logManager))
}

func verifyConcurrentServiceLogs(t *testing.T, native *Backend, spec model.SandboxSpec, logs *logging.Manager) {
	t.Helper()
	jobSpec := spec
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	newID := func(prefix string) string {
		id, err := identity.New(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	spec.SandboxID = newID("sbx")
	spec.WorkloadType, spec.RuntimeProfile.WorkloadType = model.WorkloadService, model.WorkloadService
	spec.GroupKey, spec.RuntimeProfile.ResourceClass = "service:e2e", "service:e2e"
	spec.Network.SupervisorPort, spec.Network.InspectorPort = freeTCPPort(t), freeTCPPort(t)
	for spec.Network.SupervisorPort == spec.Network.InspectorPort {
		spec.Network.InspectorPort = freeTCPPort(t)
	}
	var err error
	spec.InternalToken, err = identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	spec.ProfileHash, err = spec.RuntimeProfile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	output, err := logs.RegisterSandbox(ctx, spec.SandboxID, spec.InternalToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = native.Kill(cleanup, spec.SandboxID)
		_ = native.Delete(cleanup, spec.SandboxID)
		_ = logs.UnregisterSandbox(cleanup, spec.SandboxID)
	})
	if _, err := native.Create(ctx, spec, output); err != nil {
		t.Fatalf("create service sandbox: %v\n%s", err, readDiagnostic(logs))
	}
	client, err := supervisor.New(supervisor.Config{ProtocolVersion: protocol.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := client.Status(ctx, spec); err == nil {
			break
		} else if ctx.Err() != nil {
			t.Fatalf("service readiness: %v\n%s", err, readDiagnostic(logs))
		}
		time.Sleep(25 * time.Millisecond)
	}
	const declared = "acme/commands/logs"
	serviceID := newID("srv")
	workerIDs := []string{newID("wrk"), newID("wrk")}
	for _, workerID := range workerIDs {
		_, err := client.StartWorker(ctx, spec, supervisor.StartWorkerRequest{
			Metadata: supervisor.ExecutionMetadata{
				WorkerID: workerID, WorkloadType: model.WorkloadService, OwnerID: declared, WorkloadID: serviceID,
				ReleaseID: "active", Entrypoint: "file:///workspace/packages/acme/commands/services/logs/service.ts",
				DebuggerName: "service:" + declared + ":" + workerID, DatabaseBackend: "sqlite", DatabaseAccess: "none",
				User: execution.SystemUser(), Origin: execution.Origin{Type: "service", ID: declared},
				Service: &supervisor.ServiceExecutionMetadata{ServiceID: declared, Generation: 1, CanonicalBasePath: "/acme/commands/logs", ExecutionMode: "persistent"},
			},
			Permissions: supervisor.WorkerPermissions{Read: spec.Permissions.ReadPaths},
		})
		if err != nil {
			t.Fatalf("start service Worker: %v\n%s", err, readDiagnostic(logs))
		}
	}
	if err := client.ConfigureService(ctx, spec, serviceID, workerIDs, 2); err != nil {
		t.Fatal(err)
	}
	t.Run("service and job imports grow their shared file cache at runtime", func(t *testing.T) {
		verifySharedDenoCacheGrowth(t, client, jobSpec, spec, workerIDs[0])
	})
	type invocation struct {
		worker, username, context, parent, persistent string
	}
	position, started := logs.ReadPosition(), time.Now()
	var invocations []invocation
	for _, workerID := range workerIDs {
		for _, username := range []string{"alice", "bobby"} {
			invocations = append(invocations, invocation{workerID, username, newID("ctx"), newID("ctx"), newID("pex")})
		}
	}
	type result struct {
		value invocation
		err   error
	}
	dispatch := func(value invocation, existing bool) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://service/hold", nil)
		if err != nil {
			return err
		}
		for name, value := range map[string]string{
			"context-id": value.context, "parent-context-id": value.parent, "target-worker-id": value.worker,
			"persistent-execution-id": value.persistent, "persistent-keep-alive-ms": "30000",
			"user-id": "user:" + value.username, "username": value.username,
		} {
			request.Header.Set("the8020-internal-"+name, value)
		}
		if existing {
			request.Header.Set("the8020-internal-persistent-existing", "true")
		}
		response, err := client.DispatchService(ctx, spec, serviceID, request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return &supervisor.ResponseError{StatusCode: response.StatusCode, Status: response.Status}
		}
		var actual struct{ Username, ContextID, ParentContextID, WorkerID string }
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&actual); err != nil {
			return err
		}
		if actual.Username != value.username || actual.ContextID != value.context || actual.ParentContextID != value.parent || actual.WorkerID != value.worker {
			return fmt.Errorf("service invocation changed: %#v", actual)
		}
		return nil
	}
	finished := make(chan result, len(invocations))
	for _, value := range invocations {
		go func() { finished <- result{value, dispatch(value, false)} }()
	}
	query := records.Query{Position: position, Limit: 100, Filter: records.Filter{SandboxID: spec.SandboxID, ServiceID: serviceID, From: started}}
	waitForLogs := func(want int, completed bool) {
		t.Helper()
		for {
			page, err := logs.Query(ctx, query)
			if err != nil || page.State != "ok" {
				t.Fatalf("service log query: state=%s error=%v", page.State, err)
			}
			count := 0
			for _, value := range invocations {
				var messages []string
				for _, item := range page.Records {
					if item.ContextID != value.context {
						continue
					}
					if item.WorkerID != value.worker || item.ParentContextID != value.parent || item.Username != value.username || item.NodeID != logs.NodeID() || item.Object != "service:"+declared || item.ServiceID != serviceID || item.PersistentID != value.persistent {
						t.Fatalf("service log attribution changed: %#v", item.Record)
					}
					messages = append(messages, item.Message)
				}
				expected := []string{"service begin"}
				if completed {
					expected = append(expected, "service end")
				}
				if slices.Equal(messages, expected) {
					count++
				}
			}
			if count == want {
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("service log count %d want %d:\n%s", count, want, readDiagnostic(logs))
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	// Both users are suspended inside both Workers before any can finish.
	waitForLogs(len(invocations), false)
	for _, username := range []string{"alice", "bobby"} {
		duplicate := invocations[0]
		duplicate.username, duplicate.context = username, newID("ctx")
		var rejected *supervisor.ResponseError
		if err := dispatch(duplicate, false); !errors.As(err, &rejected) || rejected.StatusCode != http.StatusConflict {
			t.Fatalf("initial persistent identity collision was not rejected: %v", err)
		}
	}
	for _, workerID := range workerIDs {
		invocation, err := execution.NewInvocation("")
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.InvokeWorker(execution.WithInvocation(ctx, invocation), spec, workerID, "", "fixture.release", nil, execution.SystemUser())
		if err != nil || !result.OK {
			t.Fatalf("release suspended service requests: %#v, %v", result, err)
		}
	}
	for range invocations {
		result := <-finished
		if result.err != nil {
			t.Fatalf("concurrent service request %s failed: %v", result.value.context, result.err)
		}
	}
	followup := invocations[0]
	followup.context, followup.parent = newID("ctx"), followup.context
	if err := dispatch(followup, true); err != nil {
		t.Fatalf("explicit follow-up lost its original binding: %v", err)
	}
	invocations = append(invocations, followup)
	for _, workerID := range workerIDs {
		if err := client.StopWorker(ctx, spec, workerID, true); err != nil {
			t.Fatal(err)
		}
	}
	// The saved reference still retrieves each invocation after Worker cleanup.
	waitForLogs(len(invocations), true)
	t.Run("shared sources preserve old work and supply fresh Workers", func(t *testing.T) {
		verifyServiceSourceUpdate(t, client, jobSpec, spec)
	})
}

func verifySharedDenoCacheGrowth(t *testing.T, client *supervisor.Client, job, service model.SandboxSpec, workerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runtime := &commandRuntime{client: client, spec: job}
	workerManager, err := workers.New(runtime, client, 0, 64, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	jobManager, err := jobs.New(runtime, workerManager, jobs.Policy{Profile: job.RuntimeProfile, ExecutionTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer jobManager.Close()
	var requests atomic.Int32
	var online atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !online.Load() {
			http.Error(w, "dependency server is offline", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/typescript")
		_, _ = io.WriteString(w, "const answer: number = 42; export default answer;\n")
	}))
	defer server.Close()
	loadJob := func(url string) {
		t.Helper()
		record, err := jobManager.Run(ctx, "acme/commands/cache", "file:///workspace/packages/acme/commands/programs/cache/program.ts", jobs.Options{User: execution.SystemUser(), Arguments: []any{url}})
		if err != nil || fmt.Sprint(record.Result) != "42" {
			t.Fatalf("job import: %#v, %v", record, err)
		}
	}
	loadService := func(url string) {
		t.Helper()
		result, err := client.InvokeWorker(ctx, service, workerID, "", "fixture.import", url, execution.SystemUser())
		if err != nil || !result.OK || fmt.Sprint(result.Output) != "42" {
			t.Fatalf("service import: %#v, %v", result, err)
		}
	}
	var gen string
	for _, mount := range job.Mounts {
		if mount.Target == "/runtime-cache/gen" {
			gen = filepath.Join(mount.Source, "http")
		}
	}
	emits := func() map[string]time.Time {
		t.Helper()
		result := map[string]time.Time{}
		if err := filepath.Walk(gen, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				result[path] = info.ModTime()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	// Both sandboxes and the service Worker are already running before either
	// previously unknown URL is imported. Each workload can populate the cache.
	for i, pair := range [][2]func(string){{loadJob, loadService}, {loadService, loadJob}} {
		url := fmt.Sprintf("%s/late-%d.ts", server.URL, i)
		online.Store(true)
		pair[0](url)
		before := emits()
		if len(before) != i+1 {
			t.Fatalf("expected shared transpilation output: %v", before)
		}
		online.Store(false)
		pair[1](url)
		if requests.Load() != int32(i+1) || !reflect.DeepEqual(before, emits()) {
			t.Fatalf("cached import downloaded or retranspiled: requests=%d", requests.Load())
		}
	}
}

func verifySandboxHistoryLifecycle(t *testing.T, runscPath, rootFS, root string, spec model.SandboxSpec, logs *logging.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	native, err := New(Config{
		RunscPath: runscPath, RootFS: rootFS, StateRoot: filepath.Join(root, "native"), RuntimeRoot: filepath.Join(root, "runsc"),
		InstanceUUID: logs.NodeID(), KernelSocketPath: "/run/the8020/kernel.sock",
		SupervisorHeartbeatInterval: 100 * time.Millisecond, WorkerStopGrace: time.Second, StartTimeout: 15 * time.Second,
		Logger: logs.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	network, err := sandboxnetwork.NewLoopback(filepath.Join(root, "network"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := state.New(filepath.Join(root, "live"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := history.New(history.Config{Root: filepath.Join(root, "history")})
	if err != nil {
		t.Fatal(err)
	}
	client, err := supervisor.New(supervisor.Config{ProtocolVersion: protocol.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	leases, err := ports.New(filepath.Join(root, "ports"), false, logs.Logger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leases.CloseAll() })
	config := manager.Config{
		InstanceUUID: logs.NodeID(), Store: live, History: archive, Logs: logs,
		Backend: native, Network: network, Supervisor: client, Ports: leases,
		StartupTimeout: 15 * time.Second, StopGrace: time.Second, ProbeInterval: 25 * time.Millisecond,
	}
	owner, err := manager.New(config)
	if err != nil {
		t.Fatal(err)
	}
	spec.SandboxID, err = owner.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	spec.InternalToken = strings.Repeat("b", 64)
	spec.GroupKey, spec.OwnerIDs, spec.Lifecycle.Warm = "", nil, true
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = owner.Delete(cleanup, spec.SandboxID)
	})
	created, err := owner.Create(ctx, spec)
	if err != nil {
		t.Fatalf("managed creation: %v\n%s", err, readDiagnostic(logs))
	}
	if created.Status.NodeID != logs.NodeID() || created.Status.CreatedAt.IsZero() || created.Status.LogPosition == "" {
		t.Fatalf("creation lost its log reference: %#v", created.Status)
	}
	assigned, err := owner.AssignWarm(ctx, spec.SandboxID, "job:history-check", "history-owner-one")
	if err != nil || assigned.Spec.SandboxID != spec.SandboxID {
		t.Fatalf("warm assignment: %#v, %v", assigned, err)
	}
	if _, err := owner.AddOwner(ctx, spec.SandboxID, "history-owner-two"); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the authoritative state and lifecycle owner without changing
	// the native sandbox, its callback token, or its saved log boundary.
	config.Store, err = state.New(filepath.Join(root, "live"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err = manager.New(config)
	if err != nil {
		t.Fatal(err)
	}
	report, err := owner.Reconcile(ctx)
	if err != nil || report.Restored != 1 || len(report.Failed)+len(report.Missing)+len(report.OrphansDeleted) != 0 {
		t.Fatalf("reconstruct managed sandbox: %#v, %v", report, err)
	}
	restored, err := owner.Inspect(ctx, spec.SandboxID)
	if err != nil || restored.Spec.InternalToken != spec.InternalToken || restored.Status.LogPosition != created.Status.LogPosition || !restored.Status.CreatedAt.Equal(created.Status.CreatedAt) {
		t.Fatalf("reconstruction changed sandbox log identity: %#v, %v", restored.Status, err)
	}
	if destroyed, err := owner.RemoveOwner(ctx, spec.SandboxID, "history-owner-one", ""); err != nil || destroyed {
		t.Fatalf("independent owner release: %t, %v", destroyed, err)
	}
	if observation, err := native.Observe(ctx, spec.SandboxID); err != nil || observation.TaskStatus != "running" {
		t.Fatalf("remaining owner lost its sandbox: %#v, %v", observation, err)
	}
	if destroyed, err := owner.RemoveOwner(ctx, spec.SandboxID, "history-owner-two", ""); err != nil || !destroyed {
		t.Fatalf("final owner cleanup: %t, %v", destroyed, err)
	}
	if rows, err := owner.List(); err != nil || len(rows) != 0 {
		t.Fatalf("live state remains: %#v, %v", rows, err)
	}
	if objects, err := native.ListOwned(ctx); err != nil || len(objects) != 0 {
		t.Fatalf("native state remains: %#v, %v", objects, err)
	}
	if err := network.Check(ctx, spec.SandboxID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network allocation remains: %v", err)
	}
	page, err := owner.ListHistory(10, "")
	if err != nil || len(page.Sandboxes) != 1 {
		t.Fatalf("terminal metadata missing: %#v, %v", page, err)
	}
	retired, err := owner.InspectHistory(page.Sandboxes[0].HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	record := retired.Record
	if record.Spec.InternalToken != "" || record.Status.NodeID != created.Status.NodeID || record.Status.LogPosition != created.Status.LogPosition || !record.Status.CreatedAt.Equal(created.Status.CreatedAt) {
		t.Fatalf("archive changed its log reference: %#v", record.Status)
	}
	query := records.Query{Position: record.Status.LogPosition, Limit: 100, Filter: records.Filter{
		NodeID: record.Status.NodeID, SandboxID: record.Spec.SandboxID, From: record.Status.CreatedAt, Until: record.ArchivedAt,
	}}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		kernelBoot, denoBoot := false, false
		read := query
		for range 8 {
			page, err := logs.Query(ctx, read)
			if err != nil || page.State != "ok" {
				t.Fatalf("archived log query: %#v, %v", page, err)
			}
			for _, item := range page.Records {
				kernelBoot = kernelBoot || item.Source == "kernel" && item.Message == "rootless sandbox started"
				denoBoot = denoBoot || item.Source == "deno" && strings.Contains(item.Message, "Supervisor ready")
			}
			if kernelBoot && denoBoot {
				return
			}
			if !page.More {
				break
			}
			read.Position, read.Cursor = "", page.Cursor
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("retired sandbox reference could not retrieve kernel and Deno startup logs:\n%s", readDiagnostic(logs))
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

func readDiagnostic(logs *logging.Manager) string {
	page, err := logs.Query(context.Background(), records.Query{Limit: 100, Tail: true})
	if err != nil {
		return err.Error()
	}
	var text strings.Builder
	for _, record := range page.Records {
		_, _ = fmt.Fprintf(&text, "%s %s %s\n", record.Level, record.SandboxID, record.Message)
	}
	return text.String()
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
	inspection, err := r.Inspect(context.Background(), r.spec.SandboxID)
	return []manager.Inspection{inspection}, err
}

func (r *commandRuntime) ResolveSandbox(string) (model.SandboxSpec, error) {
	return r.spec, nil
}

type commandPackageSource struct {
	*workspacepackages.Store
	root string
}

func (p commandPackageSource) ActivatedPackageCommit(context.Context, string) (string, error) {
	return "active", nil
}

func commandPackages(t *testing.T) commandPackageSource {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"acme/commands/package.toml":                 "schema = 1\ndescription = \"Command fixture\"\n",
		"acme/commands/cbus/commands/arbitrary.toml": "version = 1\ncommand = \"acme.commands.check\"\nprogram = \"acme/runner/check\"\nsummary = \"Check job execution\"\nrestart_behavior = \"none\"\n",
		"acme/runner/programs/check/program.toml":    "schema = 1\ndescription = \"Check job execution\"\n",
		"acme/runner/programs/check/program.ts": `
import { context } from "@the8020/context";
import { answer } from "/p/acme/dependency/mod.ts";
export default async (...args: unknown[]) => {
  console.log("job console before await");
  if (args[0] === "fail") throw new Error("deliberate failure from command");
  const dynamic = await import("/p/acme/dependency/dynamic.ts");
  console.log("job console after await");
  await Deno.stdout.write(new TextEncoder().encode("native job stdout 💡\n"));
  await Deno.stderr.write(new TextEncoder().encode("native job stderr\n"));
  await Deno.writeTextFile("/tmp/command-check", "normal temp access");
  await Deno.writeTextFile("/runtime-cache/command-check", "normal cache access");
  const packages = [];
  for await (const entry of Deno.readDir("/workspace/packages")) packages.push(entry.name);
  return { answer: answer() + dynamic.default(), args, user: context.username, type: context.type, packages };
};
`,
		"acme/dependency/mod.ts":                   "export const answer = () => 40;\n",
		"acme/dependency/dynamic.ts":               "export default () => 2;\n",
		"acme/commands/programs/native/program.ts": "const answer: string = 42; export default () => answer;\n",
		"acme/commands/programs/cache/program.ts":  "export default async (url: string) => (await import(url)).default;\n",
		"acme/commands/modules/valid.ts":           "import { answer } from '/p/acme/dependency/mod.ts'; export const result: number = answer();\n",
		"acme/commands/modules/invalid.ts":         "export const result: number = 'deliberate native type failure';\n",
		"acme/commands/services/logs/service.ts": `
import { context } from "@the8020/context";
let release: () => void;
const gate = new Promise<void>((resolve) => release = resolve);
export async function fetch() {
  console.log("service begin");
  await gate;
  console.warn("service end");
  return Response.json({ username: context.username, contextId: context.contextId,
    parentContextId: context.parentContextId, workerId: context.workerId });
}
export const workerFunctions = {
  "fixture.release": () => { release(); return true; },
  "fixture.import": async (url: string) => (await import(url)).default,
};
`,
	} {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index := &hookPackageIndex{entries: map[string]workspacepackages.PackageIndex{
		"acme/commands": {PackageID: "acme/commands", State: "ready", ActiveCommit: "active"},
		"acme/runner":   {PackageID: "acme/runner", State: "ready", ActiveCommit: "runner-active"},
	}}
	store, err := workspacepackages.New(workspacepackages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: index})
	if err != nil {
		t.Fatal(err)
	}
	return commandPackageSource{Store: store, root: root}
}

func verifyPersistedRuntimeLogs(t *testing.T, logs *logging.Manager, sandboxID string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		page, err := logs.Query(context.Background(), records.Query{Filter: records.Filter{SandboxID: sandboxID}, Limit: 500, Tail: true})
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]records.Record{}
		beforeByContext := map[string]records.Record{}
		for _, located := range page.Records {
			r := located.Record
			switch r.Message {
			case "job console before await", "job console after await":
				if r.Username != "system" || r.Object != "program:acme/runner/check" || !identity.Is(r.WorkerID, "wrk") || !identity.Is(r.ContextID, "ctx") || !identity.Is(r.JobID, "job") || r.NodeID != "nod-0123456789" {
					t.Fatalf("incorrect managed job attribution: %#v", r)
				}
				if r.Message == "job console before await" {
					beforeByContext[r.ContextID] = r
				} else if before, ok := beforeByContext[r.ContextID]; ok {
					found[before.Message], found[r.Message] = before, r
				}
			case "native job stdout 💡", "native job stderr":
				if r.Source != "deno" || r.Stream == "" || r.WorkerID != "" || r.ContextID != "" || r.Username != "" {
					t.Fatalf("anonymous native output attribution: %#v", r)
				}
				found[r.Message] = r
			case "rootless sandbox started":
				if r.Source == "kernel" {
					found[r.Message] = r
				}
			default:
				if strings.Contains(r.Message, "deliberate failure") && strings.Contains(r.Message, "program.ts") && r.Username == "system" && identity.Is(r.ContextID, "ctx") && identity.Is(r.JobID, "job") {
					found["failed job stack"] = r
				}

			}
		}
		if len(found) == 6 {
			before, after := found["job console before await"], found["job console after await"]
			if before.ContextID != after.ContextID || before.JobID != after.JobID || before.WorkerID != after.WorkerID {
				t.Fatal("managed job lost invocation identity across await")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected kernel, console, native and failed-job records were not persisted:\n%s", readDiagnostic(logs))
}

func verifyFailedJobLogReference(t *testing.T, logs *logging.Manager, job jobs.Record) {
	t.Helper()
	query := records.Query{Position: job.LogPosition, Limit: 20, Filter: records.Filter{
		NodeID: job.NodeID, SandboxID: job.SandboxID, WorkerID: job.WorkerID,
		ContextID: job.ContextID, JobID: job.ExecutionID, From: job.QueuedAt,
	}}
	verifyFailedExecutionLogReference(t, logs, query)
}

func verifyFailedExecutionLogReference(t *testing.T, logs *logging.Manager, query records.Query) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		page, err := logs.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Records {
			if strings.Contains(item.Record.Message, "deliberate failure") && item.Record.Username == "system" {
				return
			}
		}
		if page.State != "ok" {
			t.Fatalf("failed job log reference is %s: %s", page.State, page.Reason)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("failed job reference could not retrieve its persisted diagnostics")
}

func verifyCommandJob(t *testing.T, spec model.SandboxSpec, source commandPackageSource, logs *logging.Manager) {
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
		NodeID: "nod-0123456789", LogPosition: logs.ReadPosition,
		Logger:  logs.Logger(),
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
	if report, err := indexer.Reindex(context.Background()); err != nil || report.Commands != 1 || len(report.Diagnostics) != 0 {
		t.Fatalf("command catalog=%#v error=%v", report, err)
	}
	user, _ := execution.UserForUsername("alice")
	ctx := execution.WithCaller(context.Background(), execution.Caller{User: user, ContextID: "ctx-aaaaaaaaaa", Workload: model.WorkloadService})
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
	if response.Execution == nil || response.Execution.ProgramID != "acme/runner/check" || !identity.Is(response.Execution.ExecutionID, "job") || response.Execution.SandboxID != spec.SandboxID || response.Execution.LogPosition == "" {
		t.Fatalf("real command lost log reference: %#v", response.Execution)
	}
	remaining, err := client.Workers(ctx, spec)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("job Worker cleanup: remaining=%#v error=%v", remaining, err)
	}
	t.Run("jobs execute without type checking and retain explicit dependency inspection", func(t *testing.T) {
		options := jobs.Options{User: user, DependencyModules: []string{"/workspace/packages/acme/commands/modules/valid.ts"}}
		record, err := jobManager.Run(ctx, "acme/commands/native", "file:///workspace/packages/acme/commands/programs/native/program.ts", options)
		if err != nil || fmt.Sprint(record.Result) != "42" {
			t.Fatalf("native execution/graph: %v %#v\n%s", err, record, readDiagnostic(logs))
		}
		dependencies := record.ModuleDependencies[options.DependencyModules[0]]
		if !slices.Contains(dependencies, "/workspace/packages/acme/dependency/mod.ts") {
			t.Fatalf("native graph lost imported module: %v", dependencies)
		}
		options.DependencyModules[0] = "/workspace/packages/acme/commands/modules/invalid.ts"
		record, err = jobManager.Run(ctx, "acme/commands/native", "file:///workspace/packages/acme/commands/programs/native/program.ts", options)
		if err != nil || fmt.Sprint(record.Result) != "42" || record.State != "SUCCEEDED" {
			t.Fatalf("static type errors blocked execution: %#v %v", record, err)
		}
		if remaining, err := client.Workers(ctx, spec); err != nil || len(remaining) != 0 {
			t.Fatalf("job Worker cleanup: %#v %v", remaining, err)
		}
	})
	failed := registry.Execute(ctx, core.Request{
		ProtocolVersion: core.ProtocolVersion, CommandID: registry.Catalog().Commands[0].ID, Argv: []string{"fail"},
	})
	if failed.Success || failed.Error == nil || failed.Execution == nil {
		t.Fatalf("failed command lost result identity: %#v", failed)
	}
	reference := failed.Execution
	if remaining, err := client.Workers(ctx, spec); err != nil || len(remaining) != 0 {
		t.Fatalf("failed command Worker cleanup: remaining=%#v error=%v", remaining, err)
	}
	verifyFailedExecutionLogReference(t, logs, records.Query{Position: reference.LogPosition, Limit: 20, Filter: records.Filter{
		NodeID: reference.NodeID, SandboxID: reference.SandboxID, WorkerID: reference.WorkerID,
		ContextID: reference.ContextID, JobID: reference.ExecutionID, From: reference.QueuedAt,
	}})

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

func verifyHookJob(t *testing.T, spec model.SandboxSpec, source commandPackageSource, logs *logging.Manager) {
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
	jobManager, err := jobs.New(runtime, workerManager, jobs.Policy{NodeID: "nod-0123456789", LogPosition: logs.ReadPosition, Profile: spec.RuntimeProfile, ExecutionTimeout: 10 * time.Second})
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
		if record.NodeID != "nod-0123456789" || record.SandboxID != spec.SandboxID || !identity.Is(record.WorkerID, "wrk") || !identity.Is(record.ContextID, "ctx") || !identity.Is(record.ExecutionID, "job") || record.LogPosition == "" {
			t.Fatalf("managed job lost allocated log reference: %#v", record)
		}
		if len(expectFailure) > 0 {
			verifyFailedJobLogReference(t, logs, record)
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
