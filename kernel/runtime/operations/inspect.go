package operations

import (
	"context"
	"errors"

	"the8020/kernel/identity"
	"the8020/kernel/nodes"
)

func (d *Dispatcher) ObserveExecution(ctx context.Context, executionID string) (nodes.ExecutionObservation, error) {
	if !identity.Is(executionID, "job") {
		return nodes.ExecutionObservation{}, errors.New("invalid execution ID")
	}
	runtime := d.services.RuntimeSnapshot()
	if runtime == nil || runtime.Jobs == nil || runtime.Sandboxes == nil {
		return nodes.ExecutionObservation{}, errors.New("execution runtime unavailable")
	}
	nodeID := d.services.PlatformSnapshot().Nodes.LocalNodeID()
	result := nodes.ExecutionObservation{NodeID: nodeID, ExecutionID: executionID, State: "unavailable"}
	record, err := runtime.Jobs.Inspect(executionID)
	if err != nil {
		return result, nil
	}
	result.State, result.SandboxID, result.WorkerID = record.State, record.SandboxID, record.WorkerID
	if record.SandboxID == "" || record.State != "RUNNING" {
		return result, nil
	}
	inspection, err := runtime.Sandboxes.Refresh(ctx, record.SandboxID)
	if err != nil {
		return result, err
	}
	for _, worker := range inspection.Workers {
		if worker.WorkerID == record.WorkerID {
			result.Resources = worker.Resources
			break
		}
	}
	return result, nil
}
