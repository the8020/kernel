// Package events emits local asynchronous notifications to package listeners.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
	"the8020/kernel/sandbox/model"
)

const maximumPending = 4096

type Source interface {
	EventListeners(string) []packages.EventListener
}
type Programs interface {
	RunWithOptions(context.Context, string, string, []any, map[string]string, programs.Options) (programs.Result, error)
}
type Event struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	NodeID     string    `json:"nodeId"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       any       `json:"data"`
}
type Receipt struct {
	ID        string `json:"id"`
	Listeners int    `json:"listeners"`
}
type Manager struct {
	source   Source
	programs Programs
	nodeID   string
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	pending  int
	started  bool
	wait     sync.WaitGroup
}

func New(source Source, runner Programs, nodeID string, logger *slog.Logger) (*Manager, error) {
	if source == nil || runner == nil || nodeID == "" {
		return nil, errors.New("events require packages, programs, and local node identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{source: source, programs: runner, nodeID: nodeID, logger: logger, ctx: ctx, cancel: cancel}, nil
}

func (m *Manager) Emit(name string, data any, user execution.User) (Receipt, error) {
	return m.emit(name, data, user, time.Now().UTC())
}

func (m *Manager) emit(name string, data any, user execution.User, at time.Time) (Receipt, error) {
	if len(name) > 128 || packages.ValidateName(name) != nil {
		return Receipt{}, errors.New("invalid event name")
	}
	if !user.Valid() {
		return Receipt{}, errors.New("event execution user is required")
	}
	encoded, err := json.Marshal(data)
	if err != nil || len(encoded) > 64<<10 {
		return Receipt{}, errors.New("event data must be JSON no larger than 64 KiB")
	}
	id, err := model.NewID("event")
	if err != nil {
		return Receipt{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return Receipt{}, errors.New("event dispatcher is closed")
	}
	listeners := m.source.EventListeners(name)
	if m.pending+len(listeners) > maximumPending {
		return Receipt{}, errors.New("event listener admission limit reached")
	}
	m.pending += len(listeners)
	for _, listener := range listeners {
		m.wait.Add(1)
		go func() {
			defer m.wait.Done()
			defer func() { m.mu.Lock(); m.pending--; m.mu.Unlock() }()
			var copied any
			_ = json.Unmarshal(encoded, &copied)
			event := Event{ID: id, Name: name, NodeID: m.nodeID, OccurredAt: at, Data: copied}
			_, err := m.programs.RunWithOptions(m.ctx, listener.ProgramID, listener.ProgramCommit, []any{event}, nil, programs.Options{User: user, Timeout: 10 * time.Minute})
			if err != nil && m.ctx.Err() == nil && m.logger != nil {
				m.logger.Error("event listener failed", "event", name, "event_id", id, "listener", listener.ID, "error", err)
			}
		}()
	}
	return Receipt{ID: id, Listeners: len(listeners)}, nil
}

func nextMinute(now time.Time) time.Time { return now.UTC().Truncate(time.Minute).Add(time.Minute) }

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.ctx.Err() != nil {
		return
	}
	m.started = true
	m.wait.Add(1)
	go func() {
		defer m.wait.Done()
		var last time.Time
		for {
			next := nextMinute(time.Now())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-m.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			now := time.Now().UTC()
			if now.Before(next) || !next.After(last) {
				continue
			}
			last = now.Truncate(time.Minute)
			if _, err := m.emit("minute", nil, execution.SystemUser(), last); err != nil && m.ctx.Err() == nil && m.logger != nil {
				m.logger.Error("emit minute", "error", err)
			}
		}
	}()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.wait.Wait()
	return nil
}
