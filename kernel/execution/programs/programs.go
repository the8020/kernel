// Package programs resolves active package programs and invokes them as
// ordinary one-time jobs.
package programs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
)

var ErrActiveCommitChanged = errors.New("active package commit changed")

type Resolver interface {
	ResolveProgram(context.Context, string) (workspacepackages.ProgramDefinition, error)
}

type Jobs interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
}

type Runner struct {
	programs Resolver
	jobs     Jobs
}

type Result struct {
	ProgramID   string                `json:"program_id"`
	PackageID   string                `json:"package_id"`
	Commit      string                `json:"commit"`
	ExecutionID string                `json:"execution_id"`
	Value       any                   `json:"result,omitempty"`
	Output      []supervisor.LogEvent `json:"output,omitempty"`
}

// Options selects execution identity, placement, and timeout for an ordinary job.
type Options struct {
	User         execution.User
	SandboxGroup *string
	Timeout      time.Duration
}

func New(resolver Resolver, jobRunner Jobs) (*Runner, error) {
	if resolver == nil || jobRunner == nil {
		return nil, errors.New("program resolver and job runner are required")
	}
	return &Runner{programs: resolver, jobs: jobRunner}, nil
}

// Run executes a package command as an ordinary system-user job.
func (r *Runner) Run(ctx context.Context, programID, expectedCommit string, arguments []any, secrets map[string]string) (Result, error) {
	return r.RunWithOptions(ctx, programID, expectedCommit, arguments, secrets, Options{User: execution.SystemUser()})
}

func (r *Runner) RunWithOptions(ctx context.Context, programID, expectedCommit string, arguments []any, secrets map[string]string, options Options) (Result, error) {
	program, err := r.programs.ResolveProgram(ctx, programID)
	if err != nil {
		return Result{}, err
	}
	if expectedCommit != "" && !strings.EqualFold(program.Commit, expectedCommit) {
		return Result{}, fmt.Errorf("%w: %s is now %s", ErrActiveCommitChanged, program.PackageID, program.Commit)
	}
	namespace, _, _ := strings.Cut(program.PackageID, "/")
	record, err := r.jobs.Run(ctx, program.ID, program.EntrypointURL, jobs.Options{
		User: options.User, PlacementGroup: options.SandboxGroup, Timeout: options.Timeout,
		OwnerID: program.PackageID, Arguments: arguments,
		Secrets: secrets, Namespace: namespace, ReleaseID: program.Commit,
		Origin: execution.Origin{Type: execution.OriginProgram, ID: program.ID},
	})
	result := Result{
		ProgramID: program.ID, PackageID: program.PackageID, Commit: program.Commit,
		ExecutionID: record.ExecutionID, Value: record.Result,
		Output: record.Logs,
	}
	return result, err
}
