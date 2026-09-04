// Package programs resolves active package programs and invokes them as
// ordinary one-time jobs.
package programs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/sandbox/model"
)

var ErrActiveCommitChanged = errors.New("active package commit changed")

type ExecutionError struct {
	Code    string
	Message string
	Details map[string]any
}

func (e *ExecutionError) Error() string { return e.Message }

type Resolver interface {
	SnapshotProgram(context.Context, string) (workspacepackages.ProgramDefinition, func() error, error)
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

func New(resolver Resolver, jobRunner Jobs) (*Runner, error) {
	if resolver == nil || jobRunner == nil {
		return nil, errors.New("program resolver and job runner are required")
	}
	return &Runner{programs: resolver, jobs: jobRunner}, nil
}

// Run resolves the exact active release and waits for one non-reusable job.
func (r *Runner) Run(ctx context.Context, programID, expectedCommit string, arguments []any, secrets map[string]string) (Result, error) {
	program, cleanup, err := r.programs.SnapshotProgram(ctx, programID)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = cleanup() }()
	if expectedCommit != "" && !strings.EqualFold(program.Commit, expectedCommit) {
		return Result{}, fmt.Errorf("%w: %s is now %s", ErrActiveCommitChanged, program.PackageID, program.Commit)
	}
	reuse := false
	mount := model.Mount{
		Source: program.PackageRoot, Target: packageRoot(program.PackageID), ReadOnly: true,
		OwnerScope: program.PackageID + "@" + program.Commit, Purpose: "package-program", Persistence: "execution",
	}
	record, err := r.jobs.Run(ctx, program.ID, program.EntrypointURL, jobs.Options{
		OwnerID: program.PackageID, Arguments: append([]any{}, arguments...),
		Secrets: copySecrets(secrets), Namespace: packageNamespace(program.PackageID),
		Reuse: &reuse, ReleaseID: program.Commit, DatabaseAccess: "full",
		Mounts: []model.Mount{mount},
	})
	if err != nil {
		err = redactSecureValues(err, secrets)
	}
	result := Result{
		ProgramID: program.ID, PackageID: program.PackageID, Commit: program.Commit,
		ExecutionID: record.ExecutionID, Value: record.Result,
		Output: append([]supervisor.LogEvent(nil), record.Logs...),
	}
	return result, err
}

func redactSecureValues(err error, secrets map[string]string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, value := range secrets {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[secure input]")
		}
	}
	var response *supervisor.ResponseError
	if errors.As(err, &response) && response.Code != "" {
		details := make(map[string]any, len(response.Details))
		for name, value := range response.Details {
			details[name] = redactProgramValue(value, secrets)
		}
		return &ExecutionError{Code: response.Code, Message: redactProgramText(response.Message, secrets), Details: details}
	}
	return errors.New(message)
}

func redactProgramValue(value any, secrets map[string]string) any {
	switch typed := value.(type) {
	case string:
		return redactProgramText(typed, secrets)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = redactProgramValue(typed[index], secrets)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for name, item := range typed {
			result[name] = redactProgramValue(item, secrets)
		}
		return result
	default:
		return value
	}
}

func redactProgramText(value string, secrets map[string]string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[secure input]")
		}
	}
	return value
}

func packageRoot(packageID string) string {
	return filepath.ToSlash(filepath.Join("/workspace/packages", filepath.FromSlash(packageID)))
}

func packageNamespace(packageID string) string {
	for index, character := range packageID {
		if character == '/' {
			return packageID[:index]
		}
	}
	return packageID
}

func copySecrets(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}
