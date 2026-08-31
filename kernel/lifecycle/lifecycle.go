// Package lifecycle owns the kernel's graceful-shutdown state and notification.
package lifecycle

import "sync"

// Progress is one immutable graceful lifecycle-cleanup status snapshot.
type Progress struct {
	Requested        bool
	RestartRequested bool
	Percent          int
	CompletedSteps   int
	TotalSteps       int
	Step             string
	Message          string
}

type activeStep struct {
	sequence uint64
	step     string
	message  string
}

// Manager coordinates one first-request-wins shutdown or restart request.
type Manager struct {
	once      sync.Once
	done      chan struct{}
	mu        sync.RWMutex
	total     int
	completed map[string]bool
	active    map[string]activeStep
	sequence  uint64
	progress  Progress
}

// New creates a running lifecycle manager.
func New() *Manager {
	return &Manager{done: make(chan struct{}), completed: map[string]bool{}, active: map[string]activeStep{}, progress: Progress{Step: "running", Message: "kernel is running"}}
}

// ConfigureShutdown sets the fixed number of observable graceful-shutdown
// steps before a request is accepted.
func (m *Manager) ConfigureShutdown(totalSteps int) {
	if totalSteps < 0 {
		totalSteps = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.progress.Requested {
		return
	}
	m.total = totalSteps
	m.progress.TotalSteps = totalSteps
}

// Request asks all kernel services to shut down without restarting.
func (m *Manager) Request() {
	m.request(false)
}

// RequestRestart asks all kernel services to shut down and reports whether
// restart remains the selected action. The first process-lifecycle request wins.
func (m *Manager) RequestRestart() bool {
	m.request(true)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.progress.RestartRequested
}

func (m *Manager) request(restart bool) {
	m.once.Do(func() {
		m.mu.Lock()
		m.progress.Requested = true
		m.progress.RestartRequested = restart
		m.progress.Step = "requested"
		if restart {
			m.progress.Message = "graceful restart requested"
		} else {
			m.progress.Message = "graceful shutdown requested"
		}
		m.mu.Unlock()
		close(m.done)
	})
}

// RestartRequested reports whether the accepted lifecycle request requires the
// process to replace itself after graceful cleanup.
func (m *Manager) RestartRequested() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.progress.RestartRequested
}

// Done is closed after shutdown or restart has been requested.
func (m *Manager) Done() <-chan struct{} { return m.done }

// StartStep publishes the currently active shutdown work without incrementing
// completion accounting.
func (m *Manager) StartStep(stepID, step, message string) {
	if stepID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.progress.Requested || m.completed[stepID] {
		return
	}
	m.sequence++
	m.active[stepID] = activeStep{sequence: m.sequence, step: step, message: message}
	m.progress.Step = step
	m.progress.Message = message
}

// CompleteStep marks one configured step complete. Repeated completion is
// idempotent, allowing parallel cleanup branches to report safely.
func (m *Manager) CompleteStep(stepID, step, message string) {
	if stepID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.progress.Requested {
		return
	}
	delete(m.active, stepID)
	if !m.completed[stepID] {
		m.completed[stepID] = true
		if m.total == 0 || m.progress.CompletedSteps < m.total {
			m.progress.CompletedSteps++
		}
	}
	if m.total > 0 {
		m.progress.Percent = m.progress.CompletedSteps * 100 / m.total
		if m.progress.Percent > 100 {
			m.progress.Percent = 100
		}
	}
	m.progress.Step, m.progress.Message = step, message
	var latest activeStep
	for _, candidate := range m.active {
		if candidate.sequence > latest.sequence {
			latest = candidate
		}
	}
	if latest.sequence != 0 {
		m.progress.Step, m.progress.Message = latest.step, latest.message
	}
}

// Snapshot returns one race-free progress value for status handlers.
func (m *Manager) Snapshot() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.progress
}
