package console

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/settings"
)

func setTerminalIdle(t *testing.T, m *Manager, timeout time.Duration) {
	t.Helper()
	p, err := m.Prepare(context.Background(), settings.Values{"terminal.idle_timeout": settings.Duration(timeout)})
	if err != nil {
		t.Fatal(err)
	}
	p.Commit()
}

func TestTerminalIdleCountsViewsAndRestartsAfterReattachment(t *testing.T) {
	m, terminal, opened := newRetainedTerminal(t)
	processor, err := terminal.AttachProcessor(0)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	setTerminalIdle(t, m, 200*time.Millisecond)
	assertAlive := func(duration time.Duration) {
		t.Helper()
		select {
		case <-opened.console.Done():
			t.Fatal("terminal expired while attached or before its deadline")
		case <-time.After(duration):
		}
	}
	_ = controller.Close()
	assertAlive(250 * time.Millisecond) // An observer also protects the process.
	_ = observer.Close()
	assertAlive(50 * time.Millisecond)
	controller, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	assertAlive(250 * time.Millisecond) // The previous callback cannot close a new view.
	_ = controller.Close()
	assertAlive(50 * time.Millisecond)
	select {
	case <-opened.console.Done():
	case <-time.After(time.Second):
		t.Fatal("processor prevented idle expiry")
	}
	if !processor.TerminalClosed() {
		t.Fatal("expiry did not mark physical destruction")
	}
	// Close joins cleanup, including removal of the identity and sandbox lease.
	_ = terminal.Close()
	if _, err := m.Terminal(terminal.Info().ID); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("expired terminal lookup: %v", err)
	}
}

func TestTerminalIdleIgnoresOutputAndAppliesChangedDeadline(t *testing.T) {
	m, terminal, opened := newRetainedTerminal(t)
	setTerminalIdle(t, m, time.Second)
	written := make(chan struct{})
	go func() {
		defer close(written)
		for {
			if _, err := opened.peer.Write([]byte("still producing output")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	p, err := m.Prepare(context.Background(), settings.Values{"terminal.idle_timeout": settings.Duration(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	p.Discard()
	select {
	case <-opened.console.Done():
		t.Fatal("discard changed the active timeout")
	default:
	}
	setTerminalIdle(t, m, 50*time.Millisecond) // Already past the original detach deadline.
	select {
	case <-opened.console.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("output or settings change reset idle time")
	}
	<-written
	_ = terminal.Close()
}

func TestConsoleSandboxLeaseCoversPendingOpensAndFailedCreation(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "retained"}[retained], func(t *testing.T) {
			provider := &blockedOpenProvider{entered: make(chan struct{}, 1)}
			var users atomic.Int32
			m, err := New(Config{Authentication: testAuthentication{}, Development: provider, AcquireDevelopment: func(string) (func(), error) { users.Add(1); return func() { users.Add(-1) }, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			done := make(chan error, 1)
			go func() {
				var err error
				if retained {
					_, err = m.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
				} else {
					_, err = m.OpenConsole(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
				}
				done <- err
			}()
			<-provider.entered
			if users.Load() != 1 {
				t.Fatal("pending open did not reserve sandbox lifetime")
			}
			_ = m.Close()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("opening: %v", err)
			}
			if users.Load() != 0 {
				t.Fatal("failed opening retained sandbox lifetime")
			}
		})
	}
}

func TestTerminalCreationRacingExpiryNeverReturnsMissingProcessor(t *testing.T) {
	p := &testProvider{opened: make(chan testOpen, 1)}
	m, err := New(Config{Authentication: testAuthentication{}, Development: p})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Force expiry into the creation/return window without a scheduler sleep.
	setTerminalIdle(t, m, time.Nanosecond)
	for range 100 {
		terminal, processor, err := m.CreateTerminalWithProcessor(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
		opened := <-p.opened
		if err == nil {
			if processor == nil {
				t.Fatal("creation returned a destroyed processor")
			}
			_ = terminal.Close()
		} else if !errors.Is(err, ErrTerminalGone) {
			t.Fatal(err)
		}
		_ = opened.peer.Close()
	}
}
