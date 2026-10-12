package nodes

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"the8020/kernel/sandbox/model"
)

type executionReaderFunc func(context.Context, string) (ExecutionObservation, error)

func (f executionReaderFunc) ObserveExecution(ctx context.Context, id string) (ExecutionObservation, error) {
	return f(ctx, id)
}

func TestExecutionResourcesUseTheExactAuthenticatedNode(t *testing.T) {
	owner, peer, _ := logPeers(t, nil)
	const id = "job-0123456789"
	reads := 0
	owner.SetExecutionReader(executionReaderFunc(func(_ context.Context, executionID string) (ExecutionObservation, error) {
		reads++
		if executionID != id {
			t.Fatalf("read another execution %q", executionID)
		}
		return ExecutionObservation{NodeID: owner.localID, ExecutionID: executionID, State: "RUNNING", Resources: &model.WorkerResources{HeapUsedBytes: 100, CPUUsageMicros: 200}}, nil
	}))
	for _, caller := range []*Manager{owner, peer} {
		result, err := caller.InspectExecution(context.Background(), owner.localID, id)
		if err != nil || result.Resources == nil || result.Resources.HeapUsedBytes != 100 || result.Resources.CPUUsageMicros != 200 {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	}
	if reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
	if _, err := peer.InspectExecution(context.Background(), owner.localID, "invalid"); err == nil {
		t.Fatal("accepted invalid execution ID")
	}
}

func TestExecutionInspectionRejectsWrongAndOversizedPeerReplies(t *testing.T) {
	for _, body := range []string{
		`{"node_id":"nod-aaaaaaaaaa","execution_id":"job-abcdefghij","state":"RUNNING"}`,
		`{"node_id":"nod-bbbbbbbbbb","execution_id":"job-0123456789","state":"RUNNING"}`,
		fmt.Sprintf(`{"padding":"%05000d"}`, 0),
	} {
		owner, peer, _ := logPeers(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		if _, err := peer.InspectExecution(context.Background(), owner.localID, "job-0123456789"); err == nil {
			t.Fatal("accepted invalid reply")
		}
	}
}
