package events

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
)

type source struct{ items []packages.EventListener }

func (s *source) EventListeners(event string) []packages.EventListener {
	var result []packages.EventListener
	for _, item := range s.items {
		if item.Event == event {
			result = append(result, item)
		}
	}
	return result
}

type programCall struct {
	id, commit string
	arguments  []any
	options    programs.Options
}

type runFunc func(context.Context, programCall) (programs.Result, error)

func (f runFunc) RunWithOptions(ctx context.Context, id, commit string, arguments []any, _ map[string]string, options programs.Options) (programs.Result, error) {
	return f(ctx, programCall{id, commit, arguments, options})
}

func TestEmitReturnsBeforeParallelListenersFinish(t *testing.T) {
	started := make(chan programCall, 2)
	gate := make(chan struct{})
	src := &source{[]packages.EventListener{
		{ID: "acme/one/events/first.toml", PackageID: "acme/one", Event: "example", ProgramCommit: "one", HandlerDefinition: packages.HandlerDefinition{Description: "First", ProgramID: "acme/shared/first"}},
		{ID: "acme/two/events/second.toml", PackageID: "acme/two", Event: "example", ProgramCommit: "two", HandlerDefinition: packages.HandlerDefinition{Description: "Second", ProgramID: "acme/two/second"}},
	}}
	m, _ := New(src, runFunc(func(ctx context.Context, call programCall) (programs.Result, error) {
		started <- call
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return programs.Result{}, errors.New("listener failure")
	}), "node-a", nil)
	defer m.Close()
	user, _ := execution.UserForUsername("alice")
	payload := map[string]any{"value": "before"}
	receipt := make(chan Receipt, 1)
	go func() {
		r, err := m.Emit("example", payload, user)
		if err != nil {
			t.Error(err)
		}
		receipt <- r
	}()
	select {
	case r := <-receipt:
		if r.Listeners != 2 || r.ID == "" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("Emit waited for listeners")
	}
	payload["value"] = "after"
	got := make([]programCall, 0, 2)
	for range 2 {
		select {
		case o := <-started:
			got = append(got, o)
		case <-time.After(time.Second):
			t.Fatal("listeners were serialized")
		}
	}
	expected := map[string]string{"acme/shared/first": "one", "acme/two/second": "two"}
	for _, o := range got {
		event := o.arguments[0].(Event)
		if len(o.arguments) != 1 || o.options.User != user || event.Name != "example" || event.NodeID != "node-a" || event.Data.(map[string]any)["value"] != "before" || expected[o.id] != o.commit {
			t.Fatalf("listener: %#v", o)
		}
	}
	got[0].arguments[0].(Event).Data.(map[string]any)["value"] = "mutated"
	if got[1].arguments[0].(Event).Data.(map[string]any)["value"] != "before" {
		t.Fatal("listeners share payload")
	}
	close(gate)
}

func TestRefreshAndShutdown(t *testing.T) {
	src := &source{}
	m, _ := New(src, runFunc(func(ctx context.Context, _ programCall) (programs.Result, error) {
		<-ctx.Done()
		return programs.Result{}, ctx.Err()
	}), "node", nil)
	r, err := m.Emit("minute", nil, execution.SystemUser())
	if err != nil || r.Listeners != 0 {
		t.Fatalf("empty: %v %v", r, err)
	}
	src.items = []packages.EventListener{{ID: "test", PackageID: "acme/test", Event: "minute"}}
	r, err = m.Emit("minute", nil, execution.SystemUser())
	if err != nil || r.Listeners != 1 {
		t.Fatalf("refresh: %v %v", r, err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = m.Emit("minute", nil, execution.SystemUser()) }()
	}
	_ = m.Close()
	wg.Wait()
	if _, err = m.Emit("minute", nil, execution.SystemUser()); err == nil {
		t.Fatal("accepted after shutdown")
	}
}

func TestMinuteBoundaryAndBounds(t *testing.T) {
	for _, value := range []string{"2026-09-05T12:34:00Z", "2026-09-05T12:34:59.999Z", "2026-12-31T23:59:59Z", "2026-09-05T12:34:00+02:00"} {
		now, _ := time.Parse(time.RFC3339Nano, value)
		next := nextMinute(now)
		if !next.After(now) || next.Sub(now) > time.Minute || next.Second() != 0 || next.Nanosecond() != 0 || next.Location() != time.UTC {
			t.Fatalf("boundary %v -> %v", now, next)
		}
	}
	m, _ := New(&source{}, runFunc(func(context.Context, programCall) (programs.Result, error) { return programs.Result{}, nil }), "node", nil)
	defer m.Close()
	for _, name := range []string{"", "../minute", "a/b", strings.Repeat("x", 129)} {
		if _, err := m.Emit(name, nil, execution.SystemUser()); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if _, err := m.Emit("minute", strings.Repeat("x", 65536), execution.SystemUser()); err == nil {
		t.Fatal("accepted oversized data")
	}
	if _, err := m.Emit("minute", nil, execution.User{}); err == nil {
		t.Fatal("accepted missing user")
	}
	m.pending = maximumPending
	m.source.(*source).items = []packages.EventListener{{ID: "full", Event: "minute"}}
	if _, err := m.Emit("minute", nil, execution.SystemUser()); err == nil {
		t.Fatal("accepted over capacity")
	}
}
