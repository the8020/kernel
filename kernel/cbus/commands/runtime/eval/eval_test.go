package eval

import (
	"context"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution/adminrun"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/services"
)

type repairRunner struct{ called bool }

func (r *repairRunner) Eval(context.Context, string, adminrun.Options) (adminrun.Result, error) {
	r.called = true
	return adminrun.Result{Execution: jobs.Record{State: "COMPLETED", Result: "repair executed"}}, nil
}
func (r *repairRunner) Run(context.Context, string, adminrun.Options) (adminrun.Result, error) {
	panic("unexpected run")
}

func TestApplicationFailureDoesNotGateNativeRepairExecution(t *testing.T) {
	runner := &repairRunner{}
	serviceSet := &services.Services{Runtime: &services.RuntimeServices{ApplicationFailure: "db schema package is broken", AdminRun: runner}}
	if _, err := New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"code": "export default 1"}}); err != nil || !runner.called {
		t.Fatalf("application failure blocked native eval: called=%t error=%v", runner.called, err)
	}
}
