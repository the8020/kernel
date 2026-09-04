// Package execution owns context shared across the generic execution system.
package execution

import (
	"context"

	"the8020/kernel/sandbox/model"
)

type callerKey struct{}

type Caller struct {
	ExecutionID string
	Workload    model.WorkloadType
}

// WithCaller records the validated runtime execution making a synchronous
// kernel call. Schedulers use it to avoid making child work queue behind its
// waiting parent.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	if ctx == nil || caller.ExecutionID == "" || !caller.Workload.Valid() {
		return ctx
	}
	return context.WithValue(ctx, callerKey{}, caller)
}

func CallerFromContext(ctx context.Context) (Caller, bool) {
	if ctx == nil {
		return Caller{}, false
	}
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok && caller.ExecutionID != "" && caller.Workload.Valid()
}
