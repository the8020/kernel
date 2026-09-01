package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"the8020/kernel/cbus/client"
	shutdowncommand "the8020/kernel/cbus/commands/system/shutdown"
	statuscommand "the8020/kernel/cbus/commands/system/status"
	"the8020/kernel/cbus/core"
	"the8020/kernel/instance"
	"the8020/kernel/services"
	"the8020/kernel/settings"
)

func controlPlaneDefinitions() []settings.Definition {
	minimum, maximum := int64(1), int64(65535)
	return []settings.Definition{
		{Key: "network.main_port", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(8080), Environment: "TEST_CONTROL_NETWORK_PORT", Minimum: &minimum, Maximum: &maximum, RuntimeMutable: true, Description: "Test HTTP port."},
		{Key: "network.ssh_port", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(2222), Environment: "TEST_CONTROL_SSH_PORT", Minimum: &minimum, Maximum: &maximum, RestartRequired: true, Description: "Test SSH port."},
		{Key: "logging.enabled", Type: settings.TypeBoolean, Storage: settings.StorageNode, Default: true, Environment: "TEST_CONTROL_LOGGING_ENABLED", RuntimeMutable: true, Description: "Test logging switch."},
		{Key: "logging.split_period", Type: settings.TypeEnum, Storage: settings.StorageNode, Default: "day", Environment: "TEST_CONTROL_LOGGING_SPLIT", Allowed: []string{"none", "minute", "hour", "day", "week", "month", "year"}, RuntimeMutable: true, Description: "Test log split period."},
		{Key: "logging.max_file_size", Type: settings.TypeByteSize, Storage: settings.StorageNode, Default: "1GB", Environment: "TEST_CONTROL_LOGGING_FILE", RuntimeMutable: true, Description: "Test log file limit."},
		{Key: "logging.max_total_size", Type: settings.TypeByteSize, Storage: settings.StorageNode, Default: "10GB", Environment: "TEST_CONTROL_LOGGING_TOTAL", RuntimeMutable: true, Description: "Test total log limit."},
	}
}

func registerControlPlaneCommands(registry *core.Registry, serviceSet *services.Services) error {
	commands := []struct {
		definition core.Command
		handler    core.Handler
	}{
		{core.Command{Version: 1, ID: "system.status", Path: []string{"system", "status"}}, statuscommand.New(serviceSet)},
		{core.Command{Version: 1, ID: "system.shutdown", Path: []string{"system", "shutdown"}}, shutdowncommand.New(serviceSet)},
	}
	for _, command := range commands {
		if err := registry.Register(command.definition, command.handler); err != nil {
			return err
		}
	}
	return nil
}

func TestAdministrativeSocketPrecedesRuntimeInitialization(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.WriteLayout(root, instance.Layout{
		Packages: filepath.Join(root, "packages"), Config: filepath.Join(root, "config"),
		State: filepath.Join(root, "state"), Users: filepath.Join(root, "users"),
	}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	runtimeStarted, releaseRuntime := make(chan struct{}), make(chan struct{})
	cleanupStarted, releaseCleanup := make(chan struct{}), make(chan struct{})
	config := Config{
		Root: root, Startup: map[string]string{"network.main_port": strconv.Itoa(port), "network.ssh_port": strconv.Itoa(controlPlanePort(t))}, Definitions: controlPlaneDefinitions(), Register: registerControlPlaneCommands,
		initialize: func(ctx context.Context) (*services.RuntimeServices, runtimeCleanupFunc) {
			close(runtimeStarted)
			select {
			case <-releaseRuntime:
				return &services.RuntimeServices{}, func(ctx context.Context, report shutdownProgressFunc) error {
					report(true, "runtime_controllers", "runtime controllers", "stopping runtime controllers")
					close(cleanupStarted)
					select {
					case <-releaseCleanup:
					case <-ctx.Done():
						return ctx.Err()
					}
					report(false, "runtime_controllers", "runtime controllers", "runtime controllers stopped")
					for _, step := range []struct{ id, name string }{{"runtime_ports", "runtime ports"}, {"runtime_sandboxes", "runtime sandboxes"}, {"runtime_backends", "runtime backends"}} {
						report(true, step.id, step.name, "closing "+step.name)
						report(false, step.id, step.name, step.name+" closed")
					}
					return nil
				}
			case <-ctx.Done():
				return &services.RuntimeServices{Failure: ctx.Err().Error()}, func(context.Context, shutdownProgressFunc) error { return nil }
			}
		},
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), config) }()
	select {
	case <-runtimeStarted:
	case err := <-done:
		t.Fatalf("kernel stopped before runtime initialization: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("runtime initialization did not start")
	}
	paths := instance.NewPaths(root)
	commandClient := client.New(paths.Socket)
	defer commandClient.Close()
	var status core.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err = commandClient.Execute(context.Background(), core.Request{CommandID: "system.status"})
		if err == nil && status.Success {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !status.Success {
		t.Fatalf("administrative socket unavailable while runtime initializer is blocked: response=%#v err=%v", status, err)
	}
	if status.Result["runtime_ready"] != false || status.Result["runtime_failure"] != "runtime initialization is in progress" {
		t.Fatalf("initializing status=%#v", status.Result)
	}
	close(releaseRuntime)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err = commandClient.Execute(context.Background(), core.Request{CommandID: "system.status"})
		if err == nil && status.Success && status.Result["runtime_ready"] == true {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !status.Success || status.Result["runtime_ready"] != true {
		t.Fatalf("ready status=%#v err=%v", status, err)
	}
	shutdown, err := commandClient.Execute(context.Background(), core.Request{CommandID: "system.shutdown"})
	if err != nil || !shutdown.Success {
		t.Fatalf("shutdown=%#v err=%v", shutdown, err)
	}
	select {
	case <-cleanupStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime cleanup did not start")
	}
	progress, err := commandClient.Execute(context.Background(), core.Request{CommandID: "system.status"})
	if err != nil || !progress.Success || progress.Result["shutdown_requested"] != true || progress.Result["shutdown_message"] == "" {
		t.Fatalf("shutdown progress=%#v err=%v", progress, err)
	}
	percent, ok := progress.Result["shutdown_percent"].(json.Number)
	if !ok || percent == "100" || progress.Result["shutdown_total_steps"] != json.Number("9") {
		t.Fatalf("shutdown accounting=%#v", progress.Result)
	}
	rejected, err := commandClient.Execute(context.Background(), core.Request{CommandID: "not.allowed.during.shutdown"})
	if err != nil || rejected.Error == nil || rejected.Error.Code != core.CodeShuttingDown {
		t.Fatalf("shutdown rejection=%#v err=%v", rejected, err)
	}
	close(releaseCleanup)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kernel did not shut down")
	}
}

func controlPlanePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
