// Package list implements worker.list.
package list

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	WorkerID     string `json:"worker_id"`
	WorkloadType string `json:"workload_type"`
	State        string `json:"state"`
	WorkloadID   string `json:"workload_id"`
	OwnerID      string `json:"owner_id"`
	SandboxID    string `json:"sandbox_id"`
	InFlight     int    `json:"in_flight"`
	Failure      string `json:"failure,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Workers == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "Worker manager is unavailable")
		}
		records, err := runtimeServices.Workers.List(ctx, commandutil.String(request, "sandbox_id"))
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(records))
		for _, record := range records {
			items = append(items, summary{
				WorkerID:     record.Worker.WorkerID,
				WorkloadType: string(record.WorkloadType),
				State:        record.Worker.State,
				WorkloadID:   record.Worker.WorkloadID,
				OwnerID:      record.Worker.OwnerID,
				SandboxID:    record.SandboxID,
				InFlight:     record.Worker.InFlight,
				Failure:      record.Worker.Failure,
			})
		}
		return core.Result{"workers": items}, nil
	}
}
