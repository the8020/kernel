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
	want := Caller{ExecutionID: "execution", Workload: model.WorkloadJob, User: SystemUser()}
	got, ok := CallerFromContext(WithCaller(base, want))
	if !ok || got != want {
		t.Fatalf("caller=%#v ok=%t", got, ok)
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
