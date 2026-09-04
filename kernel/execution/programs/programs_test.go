package programs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
)

type fakeResolver struct {
	program workspacepackages.ProgramDefinition
	err     error
}

func (f fakeResolver) SnapshotProgram(context.Context, string) (workspacepackages.ProgramDefinition, func() error, error) {
	return f.program, func() error { return nil }, f.err
}

type fakeJobs struct {
	jobID      string
	entrypoint string
	options    jobs.Options
	record     jobs.Record
	err        error
}

func (f *fakeJobs) Run(_ context.Context, jobID, entrypoint string, options jobs.Options) (jobs.Record, error) {
	f.jobID, f.entrypoint, f.options = jobID, entrypoint, options
	return f.record, f.err
}

func TestRunForwardsArgumentsSecretsAndDisablesReuse(t *testing.T) {
	jobsFake := &fakeJobs{record: jobs.Record{
		ExecutionID: "execution-one", Result: map[string]any{"created": true},
		Logs: []supervisor.LogEvent{{Level: "info", Message: "created"}},
	}}
	runner, err := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/users/add", PackageID: "the8020/users", Commit: "abc123",
		PackageRoot:   "/host/packages/the8020/users",
		EntrypointURL: "file:///workspace/packages/the8020/users/programs/add/program.ts",
	}}, jobsFake)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []any{"Alice Smith", "--admin"}
	secrets := map[string]string{"password": "test-password"}
	result, err := runner.Run(context.Background(), "the8020/users/add", "abc123", arguments, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if jobsFake.jobID != "the8020/users/add" || jobsFake.entrypoint == "" || jobsFake.options.OwnerID != "the8020/users" || jobsFake.options.Namespace != "the8020" || jobsFake.options.ReleaseID != "abc123" || jobsFake.options.Reuse == nil || *jobsFake.options.Reuse || jobsFake.options.DatabaseAccess != "full" {
		t.Fatalf("job call = %#v %#v", jobsFake, jobsFake.options)
	}
	if !reflect.DeepEqual(jobsFake.options.Arguments, arguments) || !reflect.DeepEqual(jobsFake.options.Secrets, secrets) || result.ExecutionID != "execution-one" || result.Value == nil || len(result.Output) != 1 {
		t.Fatalf("result=%#v options=%#v", result, jobsFake.options)
	}
	if len(jobsFake.options.Mounts) != 1 || jobsFake.options.Mounts[0].Source != "/host/packages/the8020/users" || jobsFake.options.Mounts[0].Target != "/workspace/packages/the8020/users" || !jobsFake.options.Mounts[0].ReadOnly {
		t.Fatalf("package mount = %#v", jobsFake.options.Mounts)
	}
	arguments[0], secrets["password"] = "changed", "changed"
	if jobsFake.options.Arguments[0] != "Alice Smith" || jobsFake.options.Secrets["password"] != "test-password" {
		t.Fatal("program runner retained caller-owned argument or secret containers")
	}
}

func TestRunStopsBeforeJobWhenResolutionFails(t *testing.T) {
	jobsFake := &fakeJobs{}
	runner, _ := New(fakeResolver{err: errors.New("inactive")}, jobsFake)
	if _, err := runner.Run(context.Background(), "the8020/users/add", "commit", nil, nil); err == nil {
		t.Fatal("resolution failure was ignored")
	}
	if jobsFake.jobID != "" {
		t.Fatal("job started after resolution failure")
	}
}

func TestRunNormalizesMissingArgumentsToAnEmptyArray(t *testing.T) {
	jobsFake := &fakeJobs{}
	runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/users/list", PackageID: "the8020/users", Commit: "active",
		PackageRoot: "/host/packages/the8020/users", EntrypointURL: "file:///workspace/packages/the8020/users/programs/list/program.ts",
	}}, jobsFake)
	if _, err := runner.Run(context.Background(), "the8020/users/list", "active", nil, nil); err != nil {
		t.Fatal(err)
	}
	if jobsFake.options.Arguments == nil || len(jobsFake.options.Arguments) != 0 {
		t.Fatalf("arguments=%#v", jobsFake.options.Arguments)
	}
}

func TestRunRejectsChangedCommitAndRedactsSecureErrors(t *testing.T) {
	jobsFake := &fakeJobs{err: errors.New("failed with private-password")}
	runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/users/add", PackageID: "the8020/users", Commit: "new",
		PackageRoot: "/host/packages/the8020/users", EntrypointURL: "file:///workspace/packages/the8020/users/programs/add/program.ts",
	}}, jobsFake)
	if _, err := runner.Run(context.Background(), "the8020/users/add", "old", nil, nil); !errors.Is(err, ErrActiveCommitChanged) || jobsFake.jobID != "" {
		t.Fatalf("changed commit error=%v job=%q", err, jobsFake.jobID)
	}
	if _, err := runner.Run(context.Background(), "the8020/users/add", "new", nil, map[string]string{"password": "private-password"}); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("secure job error = %v", err)
	}
}

func TestRunPreservesStructuredExecutionErrors(t *testing.T) {
	jobsFake := &fakeJobs{err: &supervisor.ResponseError{
		StatusCode: 400, Code: "invalid_arguments", Message: "invalid private-password",
		Details: map[string]any{"field": "private-password"},
	}}
	runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/services/scale", PackageID: "the8020/services", Commit: "active",
		PackageRoot: "/host/packages/the8020/services", EntrypointURL: "file:///workspace/packages/the8020/services/programs/scale/program.ts",
	}}, jobsFake)
	_, err := runner.Run(context.Background(), "the8020/services/scale", "active", nil, map[string]string{"password": "private-password"})
	var execution *ExecutionError
	if !errors.As(err, &execution) || execution.Code != "invalid_arguments" || execution.Message != "invalid [secure input]" || execution.Details["field"] != "[secure input]" {
		t.Fatalf("execution error = %#v, %v", execution, err)
	}
}
