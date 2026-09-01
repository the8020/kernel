package shared

import (
	"context"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/development"
	"the8020/kernel/services"
)

type developmentRecorder struct {
	calls             map[string]int
	activationOptions development.ActivationOptions
}

func (r *developmentRecorder) record(name string) { r.calls[name]++ }
func (r *developmentRecorder) sandbox(name string) development.Sandbox {
	r.record(name)
	return development.Sandbox{UserID: "alice", SandboxID: "dev-alice", State: development.StateReady}
}
func (r *developmentRecorder) ImageStatus() (development.ImageStatus, error) {
	r.record("image.status")
	return development.ImageStatus{BuildStatus: "ready"}, nil
}
func (r *developmentRecorder) Create(context.Context, string) (development.Sandbox, error) {
	return r.sandbox("sandbox.create"), nil
}
func (r *developmentRecorder) List() ([]development.Sandbox, error) {
	r.record("sandbox.list")
	return []development.Sandbox{{UserID: "alice", SandboxID: "dev-alice"}}, nil
}
func (r *developmentRecorder) Inspect(string) (development.Sandbox, error) {
	return r.sandbox("sandbox.inspect"), nil
}
func (r *developmentRecorder) Start(context.Context, string) (development.Sandbox, error) {
	return r.sandbox("sandbox.start"), nil
}
func (r *developmentRecorder) Stop(context.Context, string) (development.Sandbox, error) {
	return r.sandbox("sandbox.stop"), nil
}
func (r *developmentRecorder) Restart(context.Context, string) (development.Sandbox, error) {
	return r.sandbox("sandbox.restart"), nil
}
func (r *developmentRecorder) Kill(context.Context, string) (development.Sandbox, error) {
	return r.sandbox("sandbox.kill"), nil
}
func (r *developmentRecorder) Delete(context.Context, string) error {
	r.record("sandbox.delete")
	return nil
}
func (r *developmentRecorder) Shell(context.Context, string, string) (development.ShellResult, error) {
	r.record("sandbox.shell")
	return development.ShellResult{UserID: "alice", SandboxID: "dev-alice"}, nil
}
func (r *developmentRecorder) ResetSource(context.Context, string, bool) (development.Sandbox, error) {
	return r.sandbox("sandbox.reset-source"), nil
}
func (r *developmentRecorder) FactoryReset(context.Context, string, bool) (development.Sandbox, error) {
	return r.sandbox("sandbox.factory-reset"), nil
}
func (r *developmentRecorder) Preview(_ context.Context, _ string, options development.ActivationOptions) (development.ActivationPreview, error) {
	r.record("activate.preview")
	r.activationOptions = options
	return development.ActivationPreview{}, nil
}
func (r *developmentRecorder) Activate(_ context.Context, _ string, options development.ActivationOptions) (development.ActivationResult, error) {
	r.record("activate.run")
	r.activationOptions = options
	return development.ActivationResult{Success: true, Status: "committed"}, nil
}
func TestEveryDevelopmentCommandHandlerDelegatesExactlyOnce(t *testing.T) {
	recorder := &developmentRecorder{calls: map[string]int{}}
	serviceSet := &services.Services{Development: recorder}
	tests := []struct {
		name      string
		handler   core.Handler
		arguments map[string]any
	}{
		{"image.status", ImageStatus(serviceSet), nil},
		{"sandbox.create", SandboxCreate(serviceSet), map[string]any{"user_id": "developer"}},
		{"sandbox.list", SandboxList(serviceSet), nil},
		{"sandbox.inspect", SandboxInspect(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.start", SandboxStart(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.stop", SandboxStop(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.restart", SandboxRestart(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.kill", SandboxKill(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.delete", SandboxDelete(serviceSet), map[string]any{"user_id": "alice"}},
		{"sandbox.shell", SandboxShell(serviceSet), map[string]any{"user_id": "alice", "command": "pwd"}},
		{"sandbox.reset-source", SandboxResetSource(serviceSet), map[string]any{"user_id": "alice", "confirm": true}},
		{"sandbox.factory-reset", SandboxFactoryReset(serviceSet), map[string]any{"user_id": "alice", "confirm": true}},
		{"activate.preview", ActivatePreview(serviceSet), map[string]any{"user_id": "alice", "packages": "the8020/dev-core"}},
		{"activate.run", ActivateRun(serviceSet), map[string]any{"user_id": "alice", "message": "Activate", "packages": "the8020/dev-core,the8020/demo", "package_messages": `{"the8020/demo":"Override"}`, "author_name": "Developer", "author_email": "developer@example.test", "metadata": `{"client":"external-cli"}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.handler(context.Background(), core.Request{Arguments: test.arguments})
			if err != nil || len(result) == 0 || recorder.calls[test.name] != 1 {
				t.Fatalf("result=%#v calls=%d err=%v", result, recorder.calls[test.name], err)
			}
		})
	}
	if len(tests) != 14 {
		t.Fatalf("handler count = %d, want 14", len(tests))
	}
	if len(recorder.activationOptions.SelectedPackages) != 2 || recorder.activationOptions.PackageMessages["the8020/demo"] != "Override" || recorder.activationOptions.Metadata["client"] != "external-cli" {
		t.Fatalf("activation options = %#v", recorder.activationOptions)
	}
}
