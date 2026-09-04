package execution

import (
	"context"
	"testing"

	"the8020/kernel/sandbox/model"
)

func TestCallerContextRequiresCompleteValidatedIdentity(t *testing.T) {
	base := context.Background()
	if _, ok := CallerFromContext(WithCaller(base, Caller{})); ok {
		t.Fatal("accepted an incomplete caller")
	}
	want := Caller{ExecutionID: "execution", Workload: model.WorkloadJob}
	got, ok := CallerFromContext(WithCaller(base, want))
	if !ok || got != want {
		t.Fatalf("caller=%#v ok=%t", got, ok)
	}
}
