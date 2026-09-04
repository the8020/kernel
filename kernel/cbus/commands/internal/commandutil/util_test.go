package commandutil

import (
	"errors"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
)

func TestOperationErrorClassifiesInvalidServicePolicy(t *testing.T) {
	err := OperationError(errors.Join(workspacepackages.ErrInvalidServicePolicy, errors.New("invalid workers")))
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments {
		t.Fatalf("operation error = %#v, %v", commandError, err)
	}
}

func TestOperationErrorPreservesStructuredExecutionFailure(t *testing.T) {
	err := OperationError(&supervisor.ResponseError{
		Method: "POST", Path: "/v1/jobs/worker/run", Status: "400 Bad Request", StatusCode: 400,
		Code: core.CodeInvalidArguments, Message: "service ID is required", Details: map[string]any{"argument": "service-id"},
	})
	var commandError *core.Error
	if !errors.As(err, &commandError) || commandError.Code != core.CodeInvalidArguments || commandError.Message != "service ID is required" || commandError.Details["argument"] != "service-id" {
		t.Fatalf("operation error = %#v, %v", commandError, err)
	}
}
