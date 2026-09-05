package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
)

func (d *Dispatcher) execution(ctx context.Context, operation string, input map[string]any) (any, error) {
	runtime := d.services.RuntimeSnapshot()
	if runtime == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "runtime is unavailable")
	}
	body, err := json.Marshal(input)
	if err != nil || len(body) > 128<<10 {
		return nil, core.NewError(core.CodeInvalidArguments, "execution request is too large")
	}
	decode := func(target any) error {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return core.NewError(core.CodeInvalidArguments, err.Error())
		}
		return nil
	}
	user := execution.DefaultUser(ctx)
	if operation == "event.emit" {
		if runtime.Events == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "event dispatcher is unavailable")
		}
		var request struct {
			Name string `json:"name"`
			Data any    `json:"data"`
		}
		if err := decode(&request); err != nil {
			return nil, err
		}
		return runtime.Events.Emit(request.Name, request.Data, user)
	}
	if runtime.Programs == nil {
		return nil, core.NewError(core.CodeRuntimeUnavailable, "program runner is unavailable")
	}
	var request struct {
		ProgramID    string  `json:"programId"`
		Arguments    []any   `json:"arguments"`
		Username     string  `json:"username"`
		SandboxGroup *string `json:"sandboxGroup"`
		TimeoutMS    int64   `json:"timeoutMs"`
	}
	if err := decode(&request); err != nil {
		return nil, err
	}
	if request.Username != "" {
		user, err = execution.UserForUsername(request.Username)
		if err != nil {
			return nil, err
		}
	}
	if request.TimeoutMS < 0 || request.TimeoutMS > 600000 {
		return nil, core.NewError(core.CodeInvalidArguments, "timeoutMs must be between 0 and 600000")
	}
	result, runErr := runtime.Programs.RunWithOptions(ctx, request.ProgramID, "", request.Arguments, nil, programs.Options{User: user, SandboxGroup: request.SandboxGroup, Timeout: time.Duration(request.TimeoutMS) * time.Millisecond})
	state, failure := "succeeded", ""
	if runErr != nil {
		state, failure = "failed", runErr.Error()
	}
	// Execution failures retain their captured output for the Deno caller.
	return map[string]any{"state": state, "failure": failure, "executionId": result.ExecutionID, "packageCommit": result.Commit, "result": result.Value, "logs": result.Output}, nil
}
