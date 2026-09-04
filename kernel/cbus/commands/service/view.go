// Package service provides cached observed-state presentation for services.
package service

import (
	"context"
	"sync"

	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

// Observed replaces routing reservations with supervisor-observed state from
// the sandbox cache. It never performs a live supervisor or filesystem call.
func Observed(ctx context.Context, status webservices.Status, sandboxes services.SandboxService) webservices.Status {
	if sandboxes == nil {
		return status
	}
	status.Sandboxes = append([]webservices.ServiceSandboxStatus(nil), status.Sandboxes...)
	uniqueSandboxes := map[string]bool{}
	uniqueWorkers := map[string]bool{}
	for index := range status.Sandboxes {
		sandbox := &status.Sandboxes[index]
		inspection, err := sandboxes.Inspect(ctx, sandbox.RuntimeGroupID)
		if err != nil || inspection.Runtime.Revision == 0 {
			for _, workerID := range sandbox.WorkerIDs {
				uniqueWorkers[workerID] = true
			}
			uniqueSandboxes[sandbox.SandboxID] = true
			continue
		}
		sandbox.WorkerIDs = sandbox.WorkerIDs[:0]
		sandbox.ActiveRequests = 0
		sandbox.ActiveExecutions = 0
		sandbox.SnapshotRevision = inspection.Runtime.Revision
		sandbox.SnapshotObservedAt = inspection.Runtime.ObservedAt
		for _, worker := range inspection.Runtime.Workers {
			if worker.WorkloadID != sandbox.PoolID {
				continue
			}
			sandbox.WorkerIDs = append(sandbox.WorkerIDs, worker.WorkerID)
			sandbox.ActiveRequests += worker.InFlight
			active := worker.InFlight
			if worker.PersistentExecutions > active {
				active = worker.PersistentExecutions
			}
			sandbox.ActiveExecutions += active
			uniqueWorkers[worker.WorkerID] = true
		}
		uniqueSandboxes[sandbox.SandboxID] = true
	}
	status.SandboxCount = len(uniqueSandboxes)
	status.WorkerCount = len(uniqueWorkers)
	return status
}

// Refresh performs a bounded targeted refresh of only the sandboxes belonging
// to one selected service, then returns the same observed presentation.
func Refresh(ctx context.Context, status webservices.Status, sandboxes services.SandboxService) (webservices.Status, error) {
	if sandboxes == nil {
		return status, nil
	}
	seen := map[string]bool{}
	groups := make([]string, 0, len(status.Sandboxes))
	for _, sandbox := range status.Sandboxes {
		if seen[sandbox.RuntimeGroupID] {
			continue
		}
		seen[sandbox.RuntimeGroupID] = true
		groups = append(groups, sandbox.RuntimeGroupID)
	}
	if len(groups) == 0 {
		return Observed(ctx, status, sandboxes), nil
	}
	refreshContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	var workers sync.WaitGroup
	var failure error
	var failureOnce sync.Once
	for range min(8, len(groups)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for runtimeGroupID := range jobs {
				if _, err := sandboxes.Refresh(refreshContext, runtimeGroupID); err != nil {
					failureOnce.Do(func() {
						failure = err
						cancel()
					})
					return
				}
			}
		}()
	}
send:
	for _, runtimeGroupID := range groups {
		select {
		case jobs <- runtimeGroupID:
		case <-refreshContext.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if failure != nil {
		return status, failure
	}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	return Observed(ctx, status, sandboxes), nil
}
