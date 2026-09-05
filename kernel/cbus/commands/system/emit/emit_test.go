package emit

import (
	"context"
	"reflect"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/events"
	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/services"
)

type listeners struct{}

func (listeners) EventListeners(string) []packages.EventListener {
	return []packages.EventListener{{ID: "acme/app/events/notify.toml", Event: "example", HandlerDefinition: packages.HandlerDefinition{ProgramID: "acme/app/notify"}}}
}

type invocation struct {
	event events.Event
	user  execution.User
}
type programsStub struct{ started chan invocation }

func (p programsStub) RunWithOptions(ctx context.Context, _ string, _ string, arguments []any, _ map[string]string, options programs.Options) (programs.Result, error) {
	p.started <- invocation{arguments[0].(events.Event), options.User}
	<-ctx.Done()
	return programs.Result{}, ctx.Err()
}

func TestCommandEmitsDataWithoutWaitingAndRetainsCallerIdentity(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "nested"}[nested], func(t *testing.T) {
			started := make(chan invocation, 1)
			dispatcher, err := events.New(listeners{}, programsStub{started}, "node-a", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer dispatcher.Close()
			serviceSet := &services.Services{}
			serviceSet.PublishRuntime(&services.RuntimeServices{Events: dispatcher})
			ctx, user := context.Background(), execution.SystemUser()
			if nested {
				user, _ = execution.UserForUsername("alice")
				ctx = execution.WithCaller(ctx, execution.Caller{ExecutionID: "caller", Workload: model.WorkloadJob, User: user})
			}
			finished := make(chan core.Result, 1)
			go func() {
				result, err := New(serviceSet)(ctx, core.Request{Arguments: map[string]any{"event": "example", "data": `{"orderId":"123","nested":[true,42,null]}`}})
				if err != nil {
					t.Error(err)
				}
				finished <- result
			}()
			var receipt core.Result
			select {
			case receipt = <-finished:
			case <-time.After(time.Second):
				t.Fatal("emission waited for program completion")
			}
			select {
			case call := <-started:
				want := map[string]any{"orderId": "123", "nested": []any{true, float64(42), nil}}
				if receipt["listeners"] != 1 || receipt["id"] != call.event.ID || call.event.Name != "example" || call.event.NodeID != "node-a" || call.event.OccurredAt.IsZero() || call.user != user || !reflect.DeepEqual(call.event.Data, want) {
					t.Fatalf("receipt=%#v invocation=%#v", receipt, call)
				}
			case <-time.After(time.Second):
				t.Fatal("listener did not start")
			}
		})
	}
}

func TestCommandHandlesOmittedDataInvalidInputAndUnavailableRuntime(t *testing.T) {
	serviceSet := &services.Services{}
	if _, err := New(serviceSet)(context.Background(), core.Request{}); err == nil {
		t.Fatal("accepted unavailable runtime")
	}
	serviceSet.PublishRuntime(&services.RuntimeServices{})
	if _, err := New(serviceSet)(context.Background(), core.Request{}); err == nil {
		t.Fatal("accepted missing dispatcher")
	}
	started := make(chan invocation, 1)
	dispatcher, _ := events.New(listeners{}, programsStub{started}, "node", nil)
	defer dispatcher.Close()
	serviceSet.PublishRuntime(&services.RuntimeServices{Events: dispatcher})
	for _, args := range []map[string]any{{"event": "example", "data": "{"}, {"event": "example", "data": "{} {}"}, {"event": "../invalid"}} {
		if _, err := New(serviceSet)(context.Background(), core.Request{Arguments: args}); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
	if _, err := New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"event": "example"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-started:
		if call.event.Data != nil {
			t.Fatal("omitted data was not null")
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not start")
	}
}
