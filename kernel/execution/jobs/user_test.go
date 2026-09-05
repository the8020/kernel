package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"the8020/kernel/execution"
)

func TestJobPrincipalValidationDoesNotRequireAnAccount(t *testing.T) {
	coordinatorFake, workersFake := &fakeCoordinator{}, &fakeWorkers{}
	manager, err := New(coordinatorFake, workersFake, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for _, user := range []execution.User{{}, {ID: "user:alice", Username: "bob"}} {
		if _, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: user}); !errors.Is(err, execution.ErrInvalidUser) {
			t.Fatalf("invalid principal accepted: %v", err)
		}
	}
	if len(coordinatorFake.requests) != 0 || len(workersFake.starts) != 0 {
		t.Fatal("invalid principal reached execution")
	}
	for _, username := range []string{"system", "missing", "alice"} {
		user, _ := execution.UserForUsername(username)
		record, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: user})
		if err != nil || record.User != user {
			t.Fatalf("principal requires an account: %#v, %v", record, err)
		}
	}
}

func TestReusableWorkersDoNotCrossAssignedUsers(t *testing.T) {
	workersFake := &fakeWorkers{}
	policy := testPolicy()
	policy.Reuse = true
	policy.IdleRuntimeTimeout = time.Hour
	manager, err := New(&fakeCoordinator{}, workersFake, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	alice, _ := execution.UserForUsername("alice")
	bob, _ := execution.UserForUsername("bob")
	for _, user := range []execution.User{alice, bob, alice} {
		result, err := manager.Run(context.Background(), "job", "file:///programs/job.ts", Options{User: user})
		if err != nil || result.User != user {
			t.Fatalf("job identity: %#v, %v", result, err)
		}
	}
	if len(workersFake.starts) != 2 || workersFake.starts[0].Metadata.User != alice || workersFake.starts[1].Metadata.User != bob {
		t.Fatalf("user reuse boundary: %#v", workersFake.starts)
	}
}
