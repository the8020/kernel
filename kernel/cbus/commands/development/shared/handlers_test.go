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
func (r *developmentRecorder) workspace(name string) development.Workspace {
	r.record(name)
	return development.Workspace{WorkspaceID: "workspace-1", State: development.StateReady}
}
func (r *developmentRecorder) repository(name string) development.Repository {
	r.record(name)
	return development.Repository{PackageID: "the8020/dev-core", ActivationReady: true, Clean: true}
}
func (r *developmentRecorder) ImageStatus() (development.ImageStatus, error) {
	r.record("image.status")
	return development.ImageStatus{BuildStatus: "ready"}, nil
}
func (r *developmentRecorder) Create(context.Context, string) (development.Workspace, error) {
	return r.workspace("sandbox.create"), nil
}
func (r *developmentRecorder) List() ([]development.Workspace, error) {
	r.record("sandbox.list")
	return []development.Workspace{{WorkspaceID: "workspace-1"}}, nil
}
func (r *developmentRecorder) Inspect(string) (development.Workspace, error) {
	return r.workspace("sandbox.inspect"), nil
}
func (r *developmentRecorder) Start(context.Context, string) (development.Workspace, error) {
	return r.workspace("sandbox.start"), nil
}
func (r *developmentRecorder) Stop(context.Context, string) (development.Workspace, error) {
	return r.workspace("sandbox.stop"), nil
}
func (r *developmentRecorder) Restart(context.Context, string) (development.Workspace, error) {
	return r.workspace("sandbox.restart"), nil
}
func (r *developmentRecorder) Kill(context.Context, string) (development.Workspace, error) {
	return r.workspace("sandbox.kill"), nil
}
func (r *developmentRecorder) Delete(context.Context, string, bool) error {
	r.record("sandbox.delete")
	return nil
}
func (r *developmentRecorder) Shell(context.Context, string, string) (development.ShellResult, error) {
	r.record("sandbox.shell")
	return development.ShellResult{WorkspaceID: "workspace-1"}, nil
}
func (r *developmentRecorder) ResetSource(context.Context, string, bool) (development.Workspace, error) {
	return r.workspace("sandbox.reset-source"), nil
}
func (r *developmentRecorder) FactoryReset(context.Context, string, bool) (development.Workspace, error) {
	return r.workspace("sandbox.factory-reset"), nil
}
func (r *developmentRecorder) Preview(_ context.Context, _ string, options development.ActivationOptions) (development.ActivationPreview, error) {
	r.record("activate.preview")
	r.activationOptions = options
	return development.ActivationPreview{WorkspaceID: "workspace-1"}, nil
}
func (r *developmentRecorder) Activate(_ context.Context, _ string, options development.ActivationOptions) (development.ActivationResult, error) {
	r.record("activate.run")
	r.activationOptions = options
	return development.ActivationResult{WorkspaceID: "workspace-1", Success: true, Status: "committed"}, nil
}
func (r *developmentRecorder) ListRepositories() ([]development.Repository, error) {
	r.record("repository.list")
	return []development.Repository{{PackageID: "the8020/dev-core"}}, nil
}
func (r *developmentRecorder) InspectRepository(string) (development.Repository, error) {
	return r.repository("repository.inspect"), nil
}
func (r *developmentRecorder) InitializeRepository(context.Context, string, string, string, string) (development.Repository, error) {
	return r.repository("repository.init"), nil
}
func (r *developmentRecorder) ConfigureRemote(context.Context, string, string, string) (development.Repository, error) {
	return r.repository("repository.remote"), nil
}
func (r *developmentRecorder) RepositoryStatus(string) (development.Repository, error) {
	return r.repository("repository.status"), nil
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
		{"sandbox.inspect", SandboxInspect(serviceSet), map[string]any{"workspace_id": "workspace-1"}},
		{"sandbox.start", SandboxStart(serviceSet), map[string]any{"workspace_id": "workspace-1"}},
		{"sandbox.stop", SandboxStop(serviceSet), map[string]any{"workspace_id": "workspace-1"}},
		{"sandbox.restart", SandboxRestart(serviceSet), map[string]any{"workspace_id": "workspace-1"}},
		{"sandbox.kill", SandboxKill(serviceSet), map[string]any{"workspace_id": "workspace-1"}},
		{"sandbox.delete", SandboxDelete(serviceSet), map[string]any{"workspace_id": "workspace-1", "delete_home": true}},
		{"sandbox.shell", SandboxShell(serviceSet), map[string]any{"workspace_id": "workspace-1", "command": "pwd"}},
		{"sandbox.reset-source", SandboxResetSource(serviceSet), map[string]any{"workspace_id": "workspace-1", "confirm": true}},
		{"sandbox.factory-reset", SandboxFactoryReset(serviceSet), map[string]any{"workspace_id": "workspace-1", "confirm": true}},
		{"activate.preview", ActivatePreview(serviceSet), map[string]any{"workspace_id": "workspace-1", "packages": "the8020/dev-core"}},
		{"activate.run", ActivateRun(serviceSet), map[string]any{"workspace_id": "workspace-1", "message": "Activate", "packages": "the8020/dev-core,the8020/demo", "package_messages": `{"the8020/demo":"Override"}`, "author_name": "Developer", "author_email": "developer@example.test", "metadata": `{"client":"external-cli"}`}},
		{"repository.list", RepositoryList(serviceSet), nil},
		{"repository.inspect", RepositoryInspect(serviceSet), map[string]any{"package_id": "the8020/dev-core"}},
		{"repository.init", RepositoryInit(serviceSet), map[string]any{"package_id": "the8020/dev-core", "author_name": "Developer", "author_email": "developer@example.test", "message": "Initial"}},
		{"repository.remote", RepositoryRemote(serviceSet), map[string]any{"package_id": "the8020/dev-core", "name": "origin", "url": "/tmp/remote.git"}},
		{"repository.status", RepositoryStatus(serviceSet), map[string]any{"package_id": "the8020/dev-core"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.handler(context.Background(), core.Request{Arguments: test.arguments})
			if err != nil || len(result) == 0 || recorder.calls[test.name] != 1 {
				t.Fatalf("result=%#v calls=%d err=%v", result, recorder.calls[test.name], err)
			}
		})
	}
	if len(tests) != 19 {
		t.Fatalf("handler count = %d, want 19", len(tests))
	}
	if len(recorder.activationOptions.SelectedPackages) != 2 || recorder.activationOptions.PackageMessages["the8020/demo"] != "Override" || recorder.activationOptions.Metadata["client"] != "external-cli" {
		t.Fatalf("activation options = %#v", recorder.activationOptions)
	}
}
