package services

import (
	"context"
	"errors"
	"testing"

	"the8020/kernel/execution"
	"the8020/kernel/execution/records"
	"the8020/kernel/sandbox/model"
)

func TestServiceWorkerUserIsRequiredValidatedAndNotHardcoded(t *testing.T) {
	ctx := context.Background()
	store, err := records.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	visitor, _ := execution.UserForUsername("visitor")
	manager, err := New(coordinatorFake, workersFake, store, Policy{
		Strategy: model.GroupingOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := testOptions(1, 2, 1)
	for _, user := range []execution.User{{}, {ID: "user:alice", Username: "bob"}} {
		options.User = user
		if _, err := manager.Start(ctx, "pool", "file:///programs/service.ts", options); !errors.Is(err, execution.ErrInvalidUser) {
			t.Fatalf("invalid principal accepted: %v", err)
		}
	}
	if len(coordinatorFake.requests) != 0 {
		t.Fatal("invalid user provisioned a sandbox")
	}
	options.User = visitor
	record, err := manager.Start(ctx, "pool", "file:///programs/service.ts", options)
	if err != nil || record.User != visitor || len(workersFake.starts) != 1 || workersFake.starts[0].Metadata.User != visitor {
		t.Fatalf("configured user lost on Worker start: %#v, %v", record, err)
	}
	if _, err := manager.Scale(ctx, "pool", 2); err != nil || len(workersFake.starts) != 2 {
		t.Fatalf("principal could not start another Worker: %v", err)
	}
}
