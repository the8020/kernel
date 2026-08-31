package lifecycle

import (
	"sync"
	"testing"
)

func TestConcurrentShutdownProgressIsIdempotentAndBounded(t *testing.T) {
	manager := New()
	manager.ConfigureShutdown(4)
	before := manager.Snapshot()
	if before.Requested || before.Percent != 0 || before.TotalSteps != 4 || before.Step != "running" {
		t.Fatalf("before=%#v", before)
	}
	manager.Request()
	var wait sync.WaitGroup
	for _, id := range []string{"one", "two", "three", "four", "four"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			manager.StartStep(id, "working", "working on "+id)
			manager.CompleteStep(id, "complete", id+" complete")
		}()
	}
	wait.Wait()
	after := manager.Snapshot()
	if !after.Requested || after.Percent != 100 || after.CompletedSteps != 4 || after.TotalSteps != 4 {
		t.Fatalf("after=%#v", after)
	}
	manager.Request()
	select {
	case <-manager.Done():
	default:
		t.Fatal("shutdown notification was not closed")
	}
}

func TestCompletingParallelStepKeepsActiveWorkVisible(t *testing.T) {
	manager := New()
	manager.ConfigureShutdown(2)
	manager.Request()
	manager.StartStep("slow", "runtime sandboxes", "stopping runtime sandboxes")
	manager.StartStep("fast", "public HTTP", "draining public HTTP")
	manager.CompleteStep("fast", "public HTTP", "public HTTP closed")
	progress := manager.Snapshot()
	if progress.Percent != 50 || progress.Step != "runtime sandboxes" || progress.Message != "stopping runtime sandboxes" {
		t.Fatalf("progress=%#v", progress)
	}
}

func TestFirstLifecycleRequestSelectsRestartOrShutdown(t *testing.T) {
	restart := New()
	if !restart.RequestRestart() || !restart.RestartRequested() || !restart.Snapshot().RestartRequested {
		t.Fatalf("restart request was not retained: %#v", restart.Snapshot())
	}
	restart.Request()
	if !restart.RestartRequested() {
		t.Fatal("later shutdown request replaced restart")
	}

	shutdown := New()
	shutdown.Request()
	if shutdown.RequestRestart() || shutdown.RestartRequested() || shutdown.Snapshot().RestartRequested {
		t.Fatalf("later restart request replaced shutdown: %#v", shutdown.Snapshot())
	}
}
