package workers

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"the8020/kernel/execution/supervisor"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
)

// A lost startup reply does not mean the supervisor failed to create the Worker.
type accumulationControl struct {
	fakeControl
	startErr, stopErr error
	cancel            context.CancelFunc
	stopContext       error
	stoppedID         string
	deadline          bool
}

func (f *accumulationControl) StartWorker(ctx context.Context, spec model.SandboxSpec, request supervisor.StartWorkerRequest) (supervisor.WorkerStatus, error) {
	f.started = request
	if f.cancel != nil {
		f.cancel()
	}
	return supervisor.WorkerStatus{}, f.startErr
}
func (f *accumulationControl) StopWorker(ctx context.Context, spec model.SandboxSpec, id string, immediate bool) error {
	f.stopContext = ctx.Err()
	deadline, ok := ctx.Deadline()
	f.deadline = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 30*time.Second
	f.stoppedID = id
	f.stoppedIn = spec.SandboxID
	f.immediate = immediate
	return f.stopErr
}
func TestAccumulationLostStartReplyRetiresExactWorker(t *testing.T) {
	for _, test := range []struct {
		name              string
		startErr, stopErr error
	}{
		{"cancelled", context.Canceled, nil},
		{"lost response", io.ErrUnexpectedEOF, nil},
		{"cleanup failure", context.Canceled, io.ErrClosedPipe},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := &accumulationControl{startErr: test.startErr, stopErr: test.stopErr, cancel: cancel}
			spec := model.SandboxSpec{SandboxID: "sbx-aaaaaaaaaa", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
			sandboxes := &fakeSandboxes{items: []manager.Inspection{{Spec: spec}}}
			m, err := New(sandboxes, control, 0, 64, "sqlite")
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Start(ctx, spec.SandboxID, supervisor.StartWorkerRequest{Metadata: jobMetadata("wrk-aaaaaaaaaa", ""), Permissions: supervisor.WorkerPermissions{Read: []string{"/programs"}}})
			if !errors.Is(err, test.startErr) {
				t.Fatalf("lost startup cause: %v", err)
			}
			if control.stoppedID != "wrk-aaaaaaaaaa" || control.stoppedIn != spec.SandboxID || !control.immediate || control.stopContext != nil || !control.deadline {
				t.Fatalf("startup leaked Worker or inherited cancelled/unbounded cleanup: %#v", control)
			}
			if test.stopErr != nil && !errors.Is(err, test.stopErr) {
				t.Fatalf("lost cleanup cause: %v", err)
			}
			if len(m.starting) != 0 || len(m.provisional) != 0 {
				t.Fatal("startup reservation retained")
			}
		})
	}
}
func TestAccumulationRejectedStartDoesNotStopExistingWorker(t *testing.T) {
	control := &accumulationControl{startErr: &supervisor.ResponseError{StatusCode: 400, Message: "Worker already exists with different configuration"}}
	spec := model.SandboxSpec{SandboxID: "sbx-aaaaaaaaaa", WorkloadType: model.WorkloadJob, DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}}
	m, _ := New(&fakeSandboxes{items: []manager.Inspection{{Spec: spec}}}, control, 0, 64, "sqlite")
	_, err := m.Start(context.Background(), spec.SandboxID, supervisor.StartWorkerRequest{Metadata: jobMetadata("wrk-aaaaaaaaaa", ""), Permissions: supervisor.WorkerPermissions{Read: []string{"/programs"}}})
	if !supervisor.IsRequestRejected(err) || control.stoppedID != "" {
		t.Fatalf("rejection changed ownership: %v, %s", err, control.stoppedID)
	}
}
