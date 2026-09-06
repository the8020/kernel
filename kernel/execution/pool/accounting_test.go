package pool

import (
	"sync"
	"testing"
)

func TestWarmPoolAccountingAndNoReuseAfterAssignment(t *testing.T) {
	pool := NewWarmPool()
	runtimeProfile, _ := testTemplate(t)
	profileHash, _ := runtimeProfile.Hash()
	if err := pool.Resize(profileHash, 2); err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(WarmSandbox{SandboxID: "warm-1", ProfileHash: profileHash, State: WarmCreating}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetState("warm-1", WarmReady); err != nil {
		t.Fatal(err)
	}
	status := pool.Status()[0]
	if status.Ready != 1 || status.Replenish != 1 {
		t.Fatalf("initial status = %#v", status)
	}
	reserved, ok := pool.Reserve(profileHash)
	if !ok || reserved.SandboxID != "warm-1" {
		t.Fatalf("Reserve() = %#v, %v", reserved, ok)
	}
	if err := pool.SetState("warm-1", WarmAssigned); err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.Reserve(profileHash); ok {
		t.Fatal("assigned warm sandbox was reused")
	}
	if err := pool.Destroy("warm-1"); err != nil {
		t.Fatal(err)
	}
	status = pool.Status()[0]
	if status.Replenish != 2 || status.Assigned != 0 {
		t.Fatalf("released status = %#v", status)
	}
}

func TestWarmPoolRestoresDurableAccounting(t *testing.T) {
	pool := NewWarmPool()
	hash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, group := range []WarmSandbox{{SandboxID: "ready", ProfileHash: hash, State: WarmReady}, {SandboxID: "assigned", ProfileHash: hash, State: WarmAssigned}, {SandboxID: "failed", ProfileHash: hash, State: WarmFailed}} {
		if err := pool.Restore(group); err != nil {
			t.Fatal(err)
		}
	}
	status := pool.Status()[0]
	if status.Ready != 1 || status.Assigned != 1 || status.Failed != 1 {
		t.Fatalf("status=%#v", status)
	}
}

func TestWarmPoolReservationsAreAtomic(t *testing.T) {
	pool := NewWarmPool()
	runtimeProfile, _ := testTemplate(t)
	profileHash, _ := runtimeProfile.Hash()
	_ = pool.Add(WarmSandbox{SandboxID: "warm", ProfileHash: profileHash, State: WarmReady})
	var wait sync.WaitGroup
	winners := make(chan string, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if group, ok := pool.Reserve(profileHash); ok {
				winners <- group.SandboxID
			}
		}()
	}
	wait.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("reservation winners = %d, want 1", len(winners))
	}
}
