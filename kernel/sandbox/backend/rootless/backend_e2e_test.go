//go:build linux

package rootless

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	callbackServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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

	mounts := []model.Mount{
		{Source: callbackRoot, Target: "/run/the8020", ReadOnly: true, Purpose: "kernel-api", Persistence: "kernel"},
		{Target: "/tmp", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
		{Target: "/runtime-cache", MaximumSize: 64 << 20, Purpose: "temporary", Persistence: "ephemeral"},
	}
	profile := model.RuntimeProfile{
		WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DependencyMode: model.DependencyOnline, Permissions: model.Permissions{}, Mounts: mounts,
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
