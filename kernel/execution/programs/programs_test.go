package programs

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/sandbox/model"
)

type fakeResolver struct {
	program workspacepackages.ProgramDefinition
	err     error
}

func (f fakeResolver) ResolveProgram(context.Context, string) (workspacepackages.ProgramDefinition, error) {
	return f.program, f.err
}

type fakeJobs struct {
	ctx        context.Context
	jobID      string
	entrypoint string
	options    jobs.Options
	record     jobs.Record
	err        error
}

func (f *fakeJobs) Run(ctx context.Context, jobID, entrypoint string, options jobs.Options) (jobs.Record, error) {
	f.ctx, f.jobID, f.entrypoint, f.options = ctx, jobID, entrypoint, options
	return f.record, f.err
}

func TestRunSubmitsSystemJobWithDefaultRuntimePolicy(t *testing.T) {
	program := workspacepackages.ProgramDefinition{
		ID: "the8020/users/add", PackageID: "the8020/users", Commit: "abc123",
		EntrypointURL: "file:///workspace/packages/the8020/users/programs/add/program.ts",
	}
	jobRunner := &fakeJobs{record: jobs.Record{
		ExecutionID: "execution-one", Result: map[string]any{"created": true},
		Logs: []supervisor.LogEvent{{Level: "info", Message: "created"}},
	}}
	runner, err := New(fakeResolver{program: program}, jobRunner)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := execution.UserForUsername("alice")
	ctx := execution.WithCaller(context.Background(), execution.Caller{
		ExecutionID: "parent-job", Workload: model.WorkloadJob, User: user,
	})
	arguments := []any{"Alice Smith", "--admin"}
	secrets := map[string]string{"password": "test-password"}
	result, err := runner.Run(ctx, program.ID, program.Commit, arguments, secrets)
	if err != nil {
		t.Fatal(err)
	}
	wantOptions := jobs.Options{
		User: execution.SystemUser(), OwnerID: program.PackageID,
		Namespace: "the8020", ReleaseID: program.Commit,
		Arguments: arguments, Secrets: secrets,
		Origin: execution.Origin{Type: execution.OriginProgram, ID: program.ID},
	}
	if jobRunner.ctx != ctx || jobRunner.jobID != program.ID || jobRunner.entrypoint != program.EntrypointURL || !reflect.DeepEqual(jobRunner.options, wantOptions) {
		t.Fatalf("job must retain its caller context and use normal job policy: %#v", jobRunner)
	}
	wantResult := Result{
		ProgramID: program.ID, PackageID: program.PackageID, Commit: program.Commit,
		ExecutionID: jobRunner.record.ExecutionID, Value: jobRunner.record.Result, Output: jobRunner.record.Logs,
	}
	if !reflect.DeepEqual(result, wantResult) {
		t.Fatalf("result=%#v want=%#v", result, wantResult)
	}
}

func TestRunStopsBeforeJobWhenResolutionFails(t *testing.T) {
	jobRunner := &fakeJobs{}
	failure := errors.New("inactive")
	runner, _ := New(fakeResolver{err: failure}, jobRunner)
	if _, err := runner.Run(context.Background(), "the8020/users/add", "commit", nil, nil); !errors.Is(err, failure) {
		t.Fatalf("resolution error = %v", err)
	}
	if jobRunner.jobID != "" {
		t.Fatal("job started after resolution failure")
	}
}

func TestConfiguredRunPreservesUserPlacementAndTimeout(t *testing.T) {
	jobRunner := &fakeJobs{}
	runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/jobs/echo", PackageID: "the8020/jobs", Commit: "active",
		EntrypointURL: "file:///workspace/packages/the8020/jobs/programs/echo/program.ts",
	}}, jobRunner)
	user, _ := execution.UserForUsername("robot")
	group := "batch"
	_, err := runner.RunWithOptions(context.Background(), "the8020/jobs/echo", "", []any{true}, nil, Options{User: user, SandboxGroup: &group, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if jobRunner.options.User != user || jobRunner.options.PlacementGroup != &group || jobRunner.options.Timeout != time.Minute || jobRunner.options.Reuse != nil || len(jobRunner.options.Mounts) != 0 {
		t.Fatalf("job options=%#v", jobRunner.options)
	}
}

func TestRunRejectsChangedCommit(t *testing.T) {
	jobRunner := &fakeJobs{}
	runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
		ID: "the8020/users/add", PackageID: "the8020/users", Commit: "new",
		EntrypointURL: "file:///workspace/packages/the8020/users/programs/add/program.ts",
	}}, jobRunner)
	if _, err := runner.Run(context.Background(), "the8020/users/add", "old", nil, nil); !errors.Is(err, ErrActiveCommitChanged) || jobRunner.jobID != "" {
		t.Fatalf("changed commit error=%v job=%q", err, jobRunner.jobID)
	}
}

func TestRunPreservesJobErrors(t *testing.T) {
	for _, failure := range []error{
		context.Canceled,
		&supervisor.ResponseError{Code: "invalid_arguments", Message: "invalid scale", Details: map[string]any{"field": "maximum_workers"}},
	} {
		jobRunner := &fakeJobs{err: failure}
		runner, _ := New(fakeResolver{program: workspacepackages.ProgramDefinition{
			ID: "the8020/services/scale", PackageID: "the8020/services", Commit: "active",
			EntrypointURL: "file:///workspace/packages/the8020/services/programs/scale/program.ts",
		}}, jobRunner)
		if _, err := runner.Run(context.Background(), "the8020/services/scale", "active", nil, nil); err != failure {
			t.Fatalf("job error identity lost: got %v, want %v", err, failure)
		}
	}
}
