// Package list implements job.list.
package list

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	ExecutionID string `json:"execution_id"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	OwnerID     string `json:"owner_id"`
	Detached    bool   `json:"detached"`
	Duration    string `json:"duration,omitempty"`
	Failure     string `json:"failure,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Jobs == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "job manager is unavailable")
		}
		records, err := runtimeServices.Jobs.List()
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		items := make([]summary, 0, len(records))
		for _, record := range records {
			duration := ""
			if record.Duration > 0 {
				duration = record.Duration.String()
			}
			items = append(items, summary{
				ExecutionID: record.ExecutionID,
				JobID:       record.JobID,
				State:       record.State,
				OwnerID:     record.OwnerID,
				Detached:    record.Detached,
				Duration:    duration,
				Failure:     record.Failure,
			})
		}
		return core.Result{"executions": items}, nil
	}
}
