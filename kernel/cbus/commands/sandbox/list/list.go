// Package list implements sandbox.list.
package list

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	sandboxview "the8020/kernel/cbus/commands/sandbox"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	SandboxID      string `json:"sandbox_id"`
	WorkloadType   string `json:"workload_type"`
	State          string `json:"state"`
	WorkerCount    int    `json:"worker_count"`
	Warm           bool   `json:"warm"`
	RuntimeGroupID string `json:"runtime_group_id"`
	Reason         string `json:"reason"`
	Failure        string `json:"failure,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Sandboxes == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "sandbox manager is unavailable")
		}
		inspections, err := runtimeServices.Sandboxes.List()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(inspections))
		for _, inspection := range inspections {
			state := inspection.Status.ObservedState
			if state == "" {
				state = inspection.Status.DesiredState
			}
			items = append(items, summary{
				SandboxID:      inspection.Spec.SandboxID,
				WorkloadType:   string(inspection.Spec.WorkloadType),
				State:          string(state),
				WorkerCount:    inspection.Status.WorkerCount,
				Warm:           inspection.Spec.Lifecycle.Warm,
				RuntimeGroupID: inspection.Spec.RuntimeGroupID,
				Reason:         sandboxview.Reason(inspection),
				Failure:        inspection.Status.FailureReason,
			})
		}
		return core.Result{"sandboxes": items}, nil
	}
}
