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
	for _, valid := range []string{"jan_novak", "jan-novak", "a_b", "a-1"} {
		if _, err := UserForUsername(valid); err != nil {
			t.Fatalf("rejected %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"Alice", "_jan", "jan-", "-jan", "jan.novak", "ab"} {
		if _, err := UserForUsername(invalid); err == nil {
			t.Fatalf("accepted a non-canonical username %q", invalid)
		}
	}
	if !(Origin{Type: OriginService, ID: "example/api"}).ValidForWorkload(model.WorkloadService) {
		t.Fatal("rejected service origin")
	}
	if !(Origin{Type: OriginProgram, ID: "example/tool"}).ValidForWorkload(model.WorkloadJob) {
		t.Fatal("rejected program job origin")
	}
	if OriginModule != "module" || !(Origin{Type: OriginModule, ID: "evaluator"}).ValidForWorkload(model.WorkloadJob) {
		t.Fatal("rejected direct module origin")
	}
	if (Origin{Type: OriginModule, ID: "evaluator"}).ValidForWorkload(model.WorkloadService) {
		t.Fatal("accepted a module origin for a service Worker")
	}
	if (Origin{Type: "job", ID: "evaluator"}).Valid() {
		t.Fatal("accepted the obsolete job origin")
	}
}
