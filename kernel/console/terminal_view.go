package console

import (
	"context"
	"errors"
	"io"

	"the8020/kernel/sandbox/backend"
)

// TerminalViewRequest asks the canonical processor to supply an opaque display
// stream after applying at least Sequence. The kernel interprets no VT bytes.
type TerminalViewRequest struct {
	ViewID   string              `json:"viewId"`
	Sequence uint64              `json:"sequence"`
	Size     backend.ConsoleSize `json:"size"`
}

type terminalView struct {
	attachment *TerminalAttachment
	reader     *io.PipeReader
	writer     *io.PipeWriter
	accepted   bool // guarded by the terminal mutex
	sequence   uint64
	writing    chan struct{}
	ctx        context.Context
}

// OpenTerminalView attaches an existing PTY after transport authentication. It
// neither ensures a sandbox nor opens a process. Its output comes exclusively
// from the Deno display owner; replaying raw query bytes would duplicate replies.
func (m *Manager) OpenTerminalView(ctx context.Context, id, sandboxID string, size backend.ConsoleSize) (backend.Console, error) {
	t, err := m.Terminal(id)
	if err != nil {
		return nil, err
	}
	if sandboxID != "" && sandboxID != t.Info().SandboxID {
		return nil, errors.New("terminal does not belong to the selected sandbox")
	}
	t.mu.Lock()
	hasProcessor := t.processor != nil
	t.mu.Unlock()
	if !hasProcessor {
		return nil, errors.New("terminal display owner is unavailable")
	}
	a, err := t.Attach(true)
	if err != nil {
		return nil, err
	}
	if err := a.Resize(ctx, size); err != nil && !errors.Is(err, ErrTerminalGone) {
		_ = a.Close()
		return nil, err
	}
	reader, writer := io.Pipe()
	v := &terminalView{attachment: a, reader: reader, writer: writer, writing: make(chan struct{}, 1), ctx: ctx}
	t.mu.Lock()
	if _, present := t.attachments[a]; !present || t.processor == nil || t.closed {
		t.mu.Unlock()
		_ = reader.Close()
		_ = writer.Close()
		_ = a.Close()
		return nil, ErrTerminalDetached
	}
	v.sequence = t.sequence
	a.view = v
	t.notifyLocked()
	t.mu.Unlock()
	return v, nil
}

// NextView waits on attachment events, independently of PTY output sequencing.
// Only the canonical processor can accept and write display streams.
func (a *TerminalAttachment) NextView(ctx context.Context) (TerminalViewRequest, error) {
	t := a.terminal
	for {
		t.mu.Lock()
		if !a.processor || t.processor != a || t.closed {
			t.mu.Unlock()
			return TerminalViewRequest{}, ErrTerminalDetached
		}
		for other := range t.attachments {
			v := other.view
			if v != nil && !v.accepted {
				v.accepted = true
				request := TerminalViewRequest{ViewID: other.id, Sequence: v.sequence, Size: t.size}
				t.mu.Unlock()
				return request, nil
			}
		}
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return TerminalViewRequest{}, ctx.Err()
		case <-a.detached:
			return TerminalViewRequest{}, ErrTerminalDetached
		case <-changed:
		}
	}
}

func (a *TerminalAttachment) resolveView(id string) (*terminalView, error) {
	t := a.terminal
	t.mu.Lock()
	defer t.mu.Unlock()
	if !a.processor || t.processor != a || t.closed {
		return nil, ErrTerminalDetached
	}
	for other := range t.attachments {
		if other.id == id && other.view != nil && other.view.accepted {
			return other.view, nil
		}
	}
	return nil, ErrTerminalDetached
}

// WriteView acknowledges consumption of one bounded frame by the transport.
// Packages must send outside their canonical parser queue and bound slow views.
func (a *TerminalAttachment) WriteView(ctx context.Context, id string, data []byte) error {
	if len(data) == 0 || len(data) > maxFrame {
		return errors.New("terminal view frame is invalid")
	}
	v, err := a.resolveView(id)
	if err != nil {
		return err
	}
	select {
	case v.writing <- struct{}{}:
		defer func() { <-v.writing }()
	default:
		return errors.New("terminal view already has an outstanding write")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = v.Close() })
	defer stop()
	_, err = v.writer.Write(data)
	return err
}

// FinishView sends stream EOF after the final rendered bytes were consumed.
// The attachment is then released by its transport; the PTY is never closed.
func (a *TerminalAttachment) FinishView(id string) error {
	v, err := a.resolveView(id)
	if err != nil {
		return err
	}
	return v.writer.Close()
}

func (v *terminalView) Read(data []byte) (int, error) { return v.reader.Read(data) }
func (v *terminalView) Write(data []byte) (int, error) {
	if err := v.attachment.WriteFrame(v.ctx, data, false); err != nil {
		return 0, err
	}
	return len(data), nil
}
func (v *terminalView) Resize(ctx context.Context, size backend.ConsoleSize) error {
	return v.attachment.Resize(ctx, size)
}
func (v *terminalView) Close() error          { return v.attachment.Close() }
func (v *terminalView) CloseWrite() error     { return v.Close() }
func (v *terminalView) Done() <-chan struct{} { return v.attachment.detached }
func (v *terminalView) Stderr() io.Reader     { return nil }
func (v *terminalView) ExitStatus() uint32 {
	if status := v.attachment.terminal.Info().ExitStatus; status != nil {
		return *status
	}
	return 0
}

var _ backend.Console = (*terminalView)(nil)
