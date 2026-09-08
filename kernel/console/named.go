package console

import (
	"context"
	"errors"

	"the8020/kernel/identity"
	"the8020/kernel/sandbox/backend"
)

// TerminalOwner identifies the package processor; it carries no routing credential.
type TerminalOwner struct {
	NodeID                string `json:"nodeId"`
	SandboxID             string `json:"sandboxId"`
	WorkerID              string `json:"workerId"`
	PersistentExecutionID string `json:"persistentExecutionId"`
}

func ValidSessionID(id string) bool {
	if len(id) == 0 || len(id) > 40 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type OpenedTerminal struct {
	Terminal   *Terminal
	Attachment *TerminalAttachment
	Owner      TerminalOwner
	After      uint64
	Created    bool
	Reset      bool
}

// OpenTerminal serializes only competing opens of one sandbox-scoped name.
// A live processor is reused; a missing processor starts a fresh display at the
// current output boundary, preserving the shell without replaying old queries.
func (m *Manager) OpenTerminal(ctx context.Context, kind, sandboxID, sessionID string, options backend.ConsoleOptions, owner TerminalOwner) (OpenedTerminal, error) {
	if !ValidSessionID(sessionID) || !identity.Is(sandboxID, "sbx") {
		return OpenedTerminal{}, errors.New("terminal requires a sandbox and a session ID of 1..40 letters, digits, _ or -")
	}
	key := sandboxID + "\x00" + sessionID
	for {
		if err := ctx.Err(); err != nil {
			return OpenedTerminal{}, err
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return OpenedTerminal{}, ErrTerminalGone
		}
		if pending := m.namedOpening[key]; pending != nil {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return OpenedTerminal{}, ctx.Err()
			case <-pending:
				continue
			}
		}
		if len(m.namedOpening) >= maxSessions {
			m.mu.Unlock()
			return OpenedTerminal{}, errors.New("terminal opening limit reached")
		}
		pending := make(chan struct{})
		m.namedOpening[key] = pending
		var existing *Terminal
		// ponytail: at most 32 consoles; index names only if that bound grows.
		for _, t := range m.terminals {
			if t.sandboxID == sandboxID && t.sessionID == sessionID {
				existing = t
				break
			}
		}
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			delete(m.namedOpening, key)
			close(pending)
			m.mu.Unlock()
		}()
		if existing != nil {
			if existing.kind != kind {
				return OpenedTerminal{}, errors.New("terminal sandbox owner does not match")
			}
			existing.mu.Lock()
			ended := existing.closed || existing.exited
			processor := existing.processor
			var current TerminalOwner
			if processor != nil {
				current = processor.owner
			}
			existing.mu.Unlock()
			if !ended {
				if processor != nil {
					return OpenedTerminal{Terminal: existing, Owner: current}, nil
				}
				a, after, err := existing.restartProcessor(owner)
				return OpenedTerminal{Terminal: existing, Attachment: a, After: after, Reset: true}, err
			}
			if err := existing.Close(); err != nil {
				return OpenedTerminal{}, err
			}
		}
		t, err := m.createTerminal(ctx, kind, sandboxID, options, true, sessionID, owner)
		if err != nil {
			return OpenedTerminal{}, err
		}
		t.mu.Lock()
		processor, closed := t.processor, t.closed
		t.mu.Unlock()
		if closed {
			return OpenedTerminal{}, ErrTerminalGone
		}
		return OpenedTerminal{Terminal: t, Attachment: processor, Created: true}, nil
	}
}

func (t *Terminal) restartProcessor(owner TerminalOwner) (*TerminalAttachment, uint64, error) {
	id, err := identity.New("att")
	if err != nil {
		return nil, 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, err := t.attachLocked(id, false, true, t.sequence, false)
	if err != nil {
		return nil, 0, err
	}
	a.owner = owner
	return a, t.sequence, nil
}
