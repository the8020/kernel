package manager

import (
	"fmt"
	"sync"
	"testing"

	"the8020/kernel/sandbox/model"
)

// Exercise the real registry deletion used after successful idle cleanup.
func TestListDuringRetirement(t *testing.T) {
	m, store, _, _, _ := testManager(t)
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("sandbox-retired-%03d", i)
		if err := store.SaveSpec(testSandboxSpec(t, id)); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveStatus(id, model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	wait.Add(1)
	done := make(chan struct{})
	go func() {
		defer wait.Done()
		defer close(done)
		for i := 0; i < 64; i++ {
			if err := store.Delete(fmt.Sprintf("sandbox-retired-%03d", i)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer wait.Wait()
	for {
		if _, err := m.List(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

// A missing file is not retirement while the registry still owns the ID.
func TestListPreservesBrokenLiveRecord(t *testing.T) {
	m, store, _, _, _ := testManager(t)
	if err := store.SaveStatus("sandbox-broken", model.SandboxStatus{DesiredState: model.StateReady, ObservedState: model.StateReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.List(); err == nil {
		t.Fatal("missing live spec was silently hidden")
	}
}
