package console

import (
	"context"
	"errors"
	"time"

	"the8020/kernel/settings"
)

func (m *Manager) acquireSandbox(kind, sandboxID string) (func(), error) {
	if kind == "development" && m.acquireDevelopment != nil {
		return m.acquireDevelopment(sandboxID)
	}
	return func() {}, nil
}

func (t *Terminal) updateIdleLocked() {
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
	if t.closed {
		return
	}
	for a := range t.attachments {
		if !a.processor {
			t.idleSince = time.Time{}
			return
		}
	}
	if t.idleSince.IsZero() {
		t.idleSince = time.Now()
	}
	timeout := time.Duration(t.manager.idleTimeout.Load())
	if timeout > 0 {
		t.idleTimer = time.AfterFunc(time.Until(t.idleSince.Add(timeout)), t.expireIdle)
	}
}

func (t *Terminal) expireIdle() {
	t.mu.Lock()
	timeout := time.Duration(t.manager.idleTimeout.Load())
	expired := !t.closed && !t.idleSince.IsZero() && timeout > 0 && !time.Now().Before(t.idleSince.Add(timeout))
	if expired {
		t.closed = true
	} // Claim destruction before a concurrent attach.
	t.mu.Unlock()
	if expired {
		_ = t.Close()
	}
}

type idlePolicy struct {
	manager *Manager
	timeout time.Duration
}

func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	value, ok := values["terminal.idle_timeout"].(settings.Duration)
	if !ok || value <= 0 {
		return nil, errors.New("terminal.idle_timeout must be a positive duration")
	}
	return idlePolicy{m, time.Duration(value)}, nil
}

func (p idlePolicy) Discard() {}
func (p idlePolicy) Commit() {
	p.manager.idleTimeout.Store(int64(p.timeout))
	p.manager.mu.Lock()
	terminals := make([]*Terminal, 0, len(p.manager.terminals))
	for _, t := range p.manager.terminals {
		terminals = append(terminals, t)
	}
	p.manager.mu.Unlock()
	for _, t := range terminals {
		t.mu.Lock()
		t.updateIdleLocked()
		t.mu.Unlock()
	}
}
