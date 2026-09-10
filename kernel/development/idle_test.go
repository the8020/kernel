package development

import (
	"context"
	"errors"
	"testing"
	"time"

	"the8020/kernel/settings"
)

func setDevelopmentIdle(t *testing.T, m *Manager, duration time.Duration) {
	t.Helper()
	p, err := m.Prepare(context.Background(), settings.Values{"development.idle_timeout": settings.Duration(duration)})
	if err != nil {
		t.Fatal(err)
	}
	p.Commit()
}

type failingStopDriver struct {
	*fakeDriver
	failed chan struct{}
}

func (d *failingStopDriver) Stop(context.Context, string) error {
	select {
	case d.failed <- struct{}{}:
	default:
	}
	return errors.New("runtime stop unavailable")
}

func TestDevelopmentIdleStopFailurePreservesSandbox(t *testing.T) {
	p := newTestPlatform(t)
	id, err := p.manager.EnsureSandbox(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	<-p.manager.cleanupDone
	driver := &failingStopDriver{fakeDriver: p.driver, failed: make(chan struct{}, 1)}
	p.manager.driver = driver
	setDevelopmentIdle(t, p.manager, 50*time.Millisecond)
	select {
	case <-driver.failed:
	case <-time.After(time.Second):
		t.Fatal("idle stop did not run")
	}
	// Admission waits for the failed stop and cancels its retry timer.
	release, err := p.manager.AcquireConsole(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if running, err := p.driver.Running(context.Background(), id); err != nil || !running {
		t.Fatalf("failed stop destroyed private workspace: %v", err)
	}
	p.manager.driver = p.driver
}

func TestDevelopmentIdleWaitsForLastConsole(t *testing.T) {
	p := newTestPlatform(t)
	ctx := context.Background()
	id, err := p.manager.EnsureSandbox(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	acquire := func() func() {
		t.Helper()
		release, err := p.manager.AcquireConsole(id)
		if err != nil {
			t.Fatal(err)
		}
		return release
	}
	ordinary, retained := acquire(), acquire()
	setDevelopmentIdle(t, p.manager, 100*time.Millisecond)
	assertRunning := func() {
		t.Helper()
		if running, err := p.driver.Running(ctx, id); err != nil || !running {
			t.Fatalf("sandbox stopped: %v", err)
		}
	}
	ordinary()
	time.Sleep(150 * time.Millisecond)
	assertRunning()
	retained()
	time.Sleep(25 * time.Millisecond)
	reattached := acquire()
	time.Sleep(150 * time.Millisecond)
	assertRunning()
	reattached()
	deadline := time.Now().Add(3 * time.Second)
	for p.manager.HasSandbox(id) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.manager.HasSandbox(id) {
		t.Fatal("last console release did not stop sandbox")
	}
	if _, err := p.manager.Start(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	release := acquire()
	defer release()
	ordinary()
	retained()
	reattached() // Old generation releases are idempotent.
	time.Sleep(150 * time.Millisecond)
	assertRunning()
}

func TestDevelopmentIdleDoesNotInterruptShell(t *testing.T) {
	p := newTestPlatform(t)
	ctx := context.Background()
	id, err := p.manager.EnsureSandbox(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	setDevelopmentIdle(t, p.manager, 100*time.Millisecond)
	result, err := p.manager.Shell(ctx, "alice", "set -e; sleep 0.25; printf done")
	if err != nil || result.Output != "done" {
		t.Fatalf("running command interrupted: %q %v", result.Output, err)
	}
	if !p.manager.HasSandbox(id) {
		t.Fatal("running shell lost sandbox ownership")
	}
}

func TestDevelopmentIdleRejectsQueuedExpiryAndOldGenerationRelease(t *testing.T) {
	p := newTestPlatform(t)
	id, err := p.manager.EnsureSandbox(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	unlock := p.manager.lockUser("alice")
	setDevelopmentIdle(t, p.manager, 50*time.Millisecond)
	time.Sleep(75 * time.Millisecond) // Let expiry queue behind lifecycle admission.
	value, _ := p.manager.owned.Load(id)
	oldRelease, err := p.manager.acquireConsoleLocked(value.(*sandboxUse))
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.manager.Restart(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	release, err := p.manager.AcquireConsole(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	oldRelease()
	time.Sleep(100 * time.Millisecond)
	if running, err := p.driver.Running(context.Background(), id); err != nil || !running {
		t.Fatalf("stale expiry/release stopped replacement: %v", err)
	}
}
