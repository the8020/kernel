package services

import (
	"sync"
	"testing"
)

func TestRuntimePublicationReplacesCompleteSnapshots(t *testing.T) {
	pending := &RuntimeServices{Failure: "runtime initialization is in progress"}
	ready := &RuntimeServices{}
	serviceSet := &Services{Runtime: pending}
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 1_000 {
				observed := serviceSet.RuntimeSnapshot()
				if observed != pending && observed != ready {
					t.Errorf("observed partial or unknown runtime snapshot: %p", observed)
					return
				}
			}
		}()
	}
	serviceSet.PublishRuntime(ready)
	wait.Wait()
	if observed := serviceSet.RuntimeSnapshot(); observed != ready || observed.Failure != "" {
		t.Fatalf("published runtime=%#v", observed)
	}
}
