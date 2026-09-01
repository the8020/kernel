package sandbox

import (
	"testing"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

func TestReasonUsesLogicalServiceIDForListAndDetailInspections(t *testing.T) {
	summary := manager.Inspection{Spec: model.SandboxSpec{
		WorkloadType: model.WorkloadService,
		GroupKey:     "service:placement:dXVpLWxvZ2lu",
		ServiceIDs:   []string{"the8020/uui/login"},
	}}
	detail := summary
	detail.Workers = []supervisor.WorkerStatus{{OwnerID: "the8020/uui/login"}}

	const want = "service:the8020/uui/login"
	if got := Reason(summary); got != want {
		t.Fatalf("list reason = %q, want %q", got, want)
	}
	if got := Reason(detail); got != want {
		t.Fatalf("detail reason = %q, want %q", got, want)
	}
}
