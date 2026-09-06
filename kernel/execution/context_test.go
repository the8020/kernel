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
	want := Caller{ContextID: "ctx-aaaaaaaaaa", JobRunID: "job-bbbbbbbbbb", Workload: model.WorkloadJob, User: SystemUser()}
	got, ok := CallerFromContext(WithCaller(base, want))
	if !ok || got != want {
		t.Fatalf("caller=%#v ok=%t", got, ok)
	}
	for _, invalid := range []Caller{
		{ContextID: "request-1", JobRunID: want.JobRunID, Workload: want.Workload, User: want.User},
		{ContextID: want.ContextID, JobRunID: "parent-job", Workload: want.Workload, User: want.User},
		{ContextID: want.JobRunID, Workload: want.Workload, User: want.User},
	} {
		if invalid.Valid() {
			t.Fatalf("accepted malformed caller: %#v", invalid)
		}
		if _, ok := CallerFromContext(WithCaller(base, invalid)); ok {
			t.Fatalf("stored malformed caller: %#v", invalid)
		}
		if _, ok := CallerFromContext(context.WithValue(base, callerKey{}, invalid)); ok {
			t.Fatalf("read malformed caller: %#v", invalid)
		}
	}
}

func TestExecutionIdentityValidation(t *testing.T) {
	user, err := UserForUsername("alice")
	if err != nil || user != (User{ID: "user:alice", Username: "alice"}) || !user.Valid() {
		t.Fatalf("user=%#v err=%v", user, err)
	}
	if _, err := UserForUsername("Alice"); err == nil {
		t.Fatal("accepted a non-canonical username")
	}
	if !(Origin{Type: OriginService, ID: "example/api"}).ValidForWorkload(model.WorkloadService) {
		t.Fatal("rejected service origin")
	}
	if !(Origin{Type: OriginProgram, ID: "example/tool"}).ValidForWorkload(model.WorkloadJob) {
		t.Fatal("rejected program job origin")
	}
	if (Origin{Type: OriginJob, ID: "example/job"}).ValidForWorkload(model.WorkloadService) {
		t.Fatal("accepted a job origin for a service Worker")
	}
}
