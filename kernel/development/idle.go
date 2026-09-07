package development

import (
	"context"
	"errors"
	"sync"
	"time"

	"the8020/kernel/settings"
)

// One active sandbox's console ownership, including pending opens and retained PTYs.
type sandboxUse struct {
	mu                sync.Mutex
	userID, sandboxID string
	consoles          int
	idleSince         time.Time
	timer             *time.Timer
}

// AcquireConsole reserves lifetime before the provider opens a process. The
// broker releases it when its ordinary console or retained terminal closes.
func (m *Manager) AcquireConsole(sandboxID string) (func(), error) {
	value, ok := m.owned.Load(sandboxID)
	if !ok {
		return nil, errors.New("development sandbox is not running")
	}
	use := value.(*sandboxUse)
	unlock := m.lockUser(use.userID)
	defer unlock()
	return m.acquireConsoleLocked(use)
}

func (m *Manager) acquireConsoleLocked(use *sandboxUse) (func(), error) {
	use.mu.Lock()
	defer use.mu.Unlock()
	current, _ := m.owned.Load(use.sandboxID)
	if m.closed.Load() || current != use {
		return nil, errors.New("development sandbox is not running")
	}
	use.consoles++
	m.armIdleLocked(use)
	return sync.OnceFunc(func() {
		use.mu.Lock()
		defer use.mu.Unlock()
		use.consoles--
		m.armIdleLocked(use)
	}), nil
}

func (m *Manager) armIdleLocked(use *sandboxUse) {
	if use.timer != nil {
		use.timer.Stop()
		use.timer = nil
	}
	current, _ := m.owned.Load(use.sandboxID)
	if m.closed.Load() || current != use {
		return
	}
	if use.consoles != 0 {
		use.idleSince = time.Time{}
		return
	}
	if use.idleSince.IsZero() {
		use.idleSince = time.Now()
	}
	timeout := time.Duration(m.idleTimeout.Load())
	if timeout > 0 {
		use.timer = time.AfterFunc(time.Until(use.idleSince.Add(timeout)), func() { m.expireIdle(use) })
	}
}

func (m *Manager) expireIdle(use *sandboxUse) {
	unlock := m.lockUser(use.userID)
	defer unlock()
	use.mu.Lock()
	current, _ := m.owned.Load(use.sandboxID)
	timeout := time.Duration(m.idleTimeout.Load())
	expired := !m.closed.Load() && current == use && use.consoles == 0 && timeout > 0 &&
		!use.idleSince.IsZero() && !time.Now().Before(use.idleSince.Add(timeout))
	use.mu.Unlock()
	if !expired {
		return
	}
	ctx, cancel := context.WithTimeout(m.idleContext, 5*time.Minute)
	defer cancel()
	if _, err := m.stopLocked(ctx, use.userID, false); err != nil {
		if m.config.Logger != nil {
			m.config.Logger.Error("idle development sandbox stop failed", "sandbox_id", use.sandboxID, "error", err)
		}
		// Preserve the sandbox after checkpoint failure; retry after another idle interval.
		use.mu.Lock()
		use.idleSince = time.Now()
		m.armIdleLocked(use)
		use.mu.Unlock()
	}
}

// Lifecycle callers hold the user's lock. Old releases cannot affect a replacement.
func (m *Manager) forgetSandbox(id string) {
	if value, ok := m.owned.LoadAndDelete(id); ok {
		use := value.(*sandboxUse)
		use.mu.Lock()
		if use.timer != nil {
			use.timer.Stop()
			use.timer = nil
		}
		use.mu.Unlock()
	}
}

type idlePolicy struct {
	manager *Manager
	timeout time.Duration
}

func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	value, ok := values["development.idle_timeout"].(settings.Duration)
	if !ok || value <= 0 {
		return nil, errors.New("development.idle_timeout must be a positive duration")
	}
	return idlePolicy{m, time.Duration(value)}, nil
}
func (p idlePolicy) Discard() {}
func (p idlePolicy) Commit() {
	p.manager.idleTimeout.Store(int64(p.timeout))
	p.manager.owned.Range(func(_, value any) bool {
		use := value.(*sandboxUse)
		use.mu.Lock()
		p.manager.armIdleLocked(use)
		use.mu.Unlock()
		return true
	})
}
