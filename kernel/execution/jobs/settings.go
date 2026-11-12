package jobs

import (
	"context"
	"errors"
	"time"

	"the8020/kernel/settings"
)

func validateDefaults(policy Policy) error {
	if policy.MaximumParallel <= 0 || policy.QueuedExecutionLimit <= 0 || policy.ExecutionTimeout <= 0 || policy.IdleRuntimeTimeout <= 0 {
		return errors.New("job defaults must be positive")
	}
	return nil
}

// Prepare validates the five job defaults before persistence; commit wakes admission.
func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	parallel, parallelOK := values["job.default.maximum_parallel_workers"].(int64)
	queued, queuedOK := values["job.default.queued_execution_limit"].(int64)
	timeout, timeoutOK := values["job.default.execution_timeout"].(int64)
	idle, idleOK := values["job.default.idle_runtime_timeout"].(int64)
	reuse, reuseOK := values["job.default.worker_reuse"].(bool)
	if !parallelOK || !queuedOK || !timeoutOK || !idleOK || !reuseOK || parallel < 1 || queued < 1 || timeout < 1 || idle < 1 || timeout > int64((1<<63-1)/time.Millisecond) || idle > int64((1<<63-1)/time.Millisecond) {
		return nil, errors.New("invalid job default types or durations")
	}
	policy := Policy{MaximumParallel: int(parallel), QueuedExecutionLimit: int(queued), ExecutionTimeout: time.Duration(timeout) * time.Millisecond, IdleRuntimeTimeout: time.Duration(idle) * time.Millisecond, Reuse: reuse}
	if err := validateDefaults(policy); err != nil {
		return nil, err
	}
	return &preparedDefaults{manager: m, policy: policy}, nil
}

type preparedDefaults struct {
	manager *Manager
	policy  Policy
}

func (p *preparedDefaults) Commit()  { p.manager.setDefaults(p.policy) }
func (p *preparedDefaults) Discard() {}
