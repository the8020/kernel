package console

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/sandbox/backend"
)

const (
	// This is a transport recovery window, not a terminal screen or transcript.
	terminalOutputBytes  = 1 << 20
	terminalOutputEvents = 1024
	terminalReadBytes    = 256 << 10
	terminalInputFrames  = 16
	terminalAttachments  = 4
)

var (
	ErrTerminalGone          = errors.New("terminal is unavailable")
	ErrTerminalBusy          = errors.New("terminal already has an input controller")
	ErrTerminalDetached      = errors.New("terminal attachment is detached")
	ErrTerminalReadOnly      = errors.New("terminal attachment does not control input")
	ErrTerminalOutputGap     = errors.New("terminal output is no longer retained; a display snapshot is required")
	ErrTerminalInputFull     = errors.New("terminal input queue is full")
	ErrTerminalProcessorBusy = errors.New("terminal already has an output processor")
)

// TerminalInfo is physical process state. Names and display state belong to Deno.
type TerminalInfo struct {
	ID         string              `json:"id"`
	Kind       string              `json:"kind"`
	SandboxID  string              `json:"sandboxId"`
	Size       backend.ConsoleSize `json:"size"`
	Sequence   uint64              `json:"sequence"`
	Exited     bool                `json:"exited"`
	ExitStatus *uint32             `json:"exitStatus,omitempty"`
}

// TerminalEvent orders raw output and successful geometry changes. Data remains
// opaque to the kernel; a Deno terminal engine interprets it exactly once.
type TerminalEvent struct {
	Sequence uint64               `json:"sequence"`
	Data     []byte               `json:"data,omitempty"`
	Size     *backend.ConsoleSize `json:"size,omitempty"`
}

type TerminalBatch struct {
	Events     []TerminalEvent `json:"events"`
	Sequence   uint64          `json:"sequence"`
	Exited     bool            `json:"exited"`
	ExitStatus *uint32         `json:"exitStatus,omitempty"`
}

// Terminal owns a retained PTY. Neither a creating request nor an attachment
// owns its lifetime. Close, process exit, and provider/broker shutdown end it.
type Terminal struct {
	manager      *Manager
	id           string
	kind         string
	sandboxID    string
	console      backend.Console
	cancel       context.CancelFunc
	closeOnce    sync.Once
	mu           sync.Mutex
	size         backend.ConsoleSize
	events       []TerminalEvent
	bytes        int
	sequence     uint64
	changed      chan struct{}
	exited       bool
	exitStatus   *uint32
	closed       bool
	controller   *TerminalAttachment
	processor    *TerminalAttachment
	processed    uint64
	pendingBytes int
	attachments  map[*TerminalAttachment]struct{}
	input        chan terminalInput
	inputError   error
	done         chan struct{}
	release      func()
	idleSince    time.Time
	idleTimer    *time.Timer
}

type terminalInput struct {
	data   []byte
	result chan error
}

// TerminalAttachment is a replaceable transport lease. A terminal permits one
// input/resize controller and bounded observers. Closing a lease only detaches.
type TerminalAttachment struct {
	terminal  *Terminal
	id        string
	control   bool
	processor bool
	detached  chan struct{}
	view      *terminalView
}

// CreateTerminal opens a native PTY with broker lifetime. Cancellation while the
// provider is opening still cancels creation; cancellation after registration
// cannot terminate the process.
func (m *Manager) CreateTerminal(ctx context.Context, kind, sandboxID string, options backend.ConsoleOptions) (*Terminal, error) {
	return m.createTerminal(ctx, kind, sandboxID, options, false)
}

// CreateTerminalWithProcessor establishes the lossless output owner before the
// first PTY read. Deno uses this owner for terminal state and query responses;
// browser/SSH views remain independent replaceable attachments.
func (m *Manager) CreateTerminalWithProcessor(ctx context.Context, kind, sandboxID string, options backend.ConsoleOptions) (*Terminal, *TerminalAttachment, error) {
	t, err := m.createTerminal(ctx, kind, sandboxID, options, true)
	if err != nil {
		return nil, nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, nil, ErrTerminalGone
	}
	return t, t.processor, nil
}

func (m *Manager) createTerminal(ctx context.Context, kind, sandboxID string, options backend.ConsoleOptions, processOutput bool) (*Terminal, error) {
	if !validTarget(target{Kind: kind, SandboxID: sandboxID}) || !options.Terminal {
		return nil, errors.New("persistent terminals require a valid target and PTY")
	}
	if err := backend.ValidateConsoleOptions(options); err != nil {
		return nil, err
	}
	id, err := identity.New("tty")
	if err != nil {
		return nil, err
	}
	provider, err := m.reserveProvider(kind)
	if err != nil {
		return nil, err
	}
	defer m.releaseOpening()
	release, err := m.acquireSandbox(kind, sandboxID)
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	ownerCtx, cancel := context.WithCancel(m.lifetime)
	stopCancel := context.AfterFunc(ctx, cancel)
	value, err := provider.OpenConsole(ownerCtx, sandboxID, options)
	if err != nil {
		stopCancel()
		cancel()
		return nil, err
	}
	if !stopCancel() || ctx.Err() != nil {
		cancel()
		_ = value.Close()
		return nil, ctx.Err()
	}
	terminal := &Terminal{
		manager: m, id: id, kind: kind, sandboxID: sandboxID, console: value,
		cancel: cancel, size: options.Size, changed: make(chan struct{}),
		attachments: make(map[*TerminalAttachment]struct{}),
		input:       make(chan terminalInput, terminalInputFrames), done: make(chan struct{}),
		release: release,
	}
	if processOutput {
		if _, err := terminal.AttachProcessor(0); err != nil {
			cancel()
			_ = value.Close()
			return nil, err
		}
	}
	m.mu.Lock()
	switch {
	case m.closed:
		err = errors.New("console broker is closed")
	case kind == "runtime" && m.runtime != provider:
		err = errors.New("runtime sandbox consoles are not available")
	case len(m.sessions)+len(m.terminals) >= maxSessions:
		err = errors.New("console session limit reached")
	case m.terminals[id] != nil:
		err = errors.New("terminal identity collision")
	default:
		m.terminals[id] = terminal
	}
	m.mu.Unlock()
	if err != nil {
		cancel()
		_ = value.Close()
		return nil, err
	}
	retained = true
	terminal.mu.Lock()
	terminal.updateIdleLocked()
	terminal.mu.Unlock()
	go terminal.readOutput()
	go terminal.writeInput()
	return terminal, nil
}

// Terminal resolves an identity after the caller's transport authorization.
// Operational IDs are not credentials. This never creates a replacement PTY.
func (m *Manager) Terminal(id string) (*Terminal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value := m.terminals[id]
	if m.closed || value == nil {
		return nil, ErrTerminalGone
	}
	return value, nil
}

func (m *Manager) Terminals(kind, sandboxID string) []TerminalInfo {
	m.mu.Lock()
	values := make([]*Terminal, 0)
	for _, value := range m.terminals {
		if value.kind == kind && value.sandboxID == sandboxID {
			values = append(values, value)
		}
	}
	m.mu.Unlock()
	result := make([]TerminalInfo, 0, len(values))
	for _, value := range values {
		result = append(result, value.Info())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (t *Terminal) Info() TerminalInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TerminalInfo{ID: t.id, Kind: t.kind, SandboxID: t.sandboxID, Size: t.size,
		Sequence: t.sequence, Exited: t.exited, ExitStatus: t.statusLocked()}
}

func (t *Terminal) statusLocked() *uint32 {
	if t.exitStatus == nil {
		return nil
	}
	status := *t.exitStatus
	return &status
}

func (t *Terminal) Attach(control bool) (*TerminalAttachment, error) {
	return t.attach(control, false, 0, false)
}

// TakeControl explicitly replaces the input/resize lease, including an SSH
// view. Accepted input still drains through the same ordered native writer.
func (t *Terminal) TakeControl() (*TerminalAttachment, error) {
	return t.attach(true, false, 0, true)
}

// AttachProcessor resumes the sole output interpreter from its applied sequence.
// Its acknowledgement gates PTY ingestion rather than dropping uninterpreted
// bytes. This is the terminal engine, not a browser transport or slow view.
func (t *Terminal) AttachProcessor(after uint64) (*TerminalAttachment, error) {
	return t.attach(false, true, after, false)
}

func (t *Terminal) attach(control, processor bool, after uint64, takeover bool) (*TerminalAttachment, error) {
	id, err := identity.New("att")
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, ErrTerminalGone
	}
	if processor {
		if t.processor != nil {
			return nil, ErrTerminalProcessorBusy
		}
		if after > t.sequence {
			return nil, errors.New("terminal sequence is in the future")
		}
		if len(t.events) > 0 && after < t.events[0].Sequence-1 {
			return nil, ErrTerminalOutputGap
		}
	}
	if control && t.controller != nil {
		if !takeover {
			return nil, ErrTerminalBusy
		}
		t.controller.closeLocked()
	}
	if len(t.attachments) >= terminalAttachments {
		return nil, errors.New("terminal attachment limit reached")
	}
	value := &TerminalAttachment{terminal: t, id: id, control: control, processor: processor, detached: make(chan struct{})}
	t.attachments[value] = struct{}{}
	if control {
		t.controller = value
	}
	if processor {
		t.processor = value
		t.processed = after
		t.pendingBytes = 0
		for _, event := range t.events {
			if event.Sequence > after {
				t.pendingBytes += len(event.Data)
			}
		}
	}
	if !processor {
		t.updateIdleLocked()
	}
	return value, nil
}

func (a *TerminalAttachment) ID() string { return a.id }

func (a *TerminalAttachment) Close() error {
	a.terminal.mu.Lock()
	a.closeLocked()
	a.terminal.mu.Unlock()
	return nil
}

func (a *TerminalAttachment) closeLocked() {
	t := a.terminal
	if _, present := t.attachments[a]; !present {
		return
	}
	delete(t.attachments, a)
	if t.controller == a {
		t.controller = nil
	}
	if t.processor == a {
		t.processor = nil
		t.pendingBytes = 0
		for other := range t.attachments {
			if other.view != nil {
				other.closeLocked()
			}
		}
	}
	if a.view != nil {
		_ = a.view.reader.CloseWithError(ErrTerminalDetached)
		_ = a.view.writer.CloseWithError(ErrTerminalDetached)
	}
	close(a.detached)
	t.notifyLocked()
	if !a.processor {
		t.updateIdleLocked()
	}
}

// Read waits for new sequenced events without polling. A lagging consumer gets
// an explicit gap error, never an apparently contiguous truncated transcript.
func (a *TerminalAttachment) Read(ctx context.Context, after uint64) (TerminalBatch, error) {
	t := a.terminal
	for {
		t.mu.Lock()
		if _, ok := t.attachments[a]; !ok {
			t.mu.Unlock()
			return TerminalBatch{}, ErrTerminalDetached
		}
		if t.closed {
			t.mu.Unlock()
			return TerminalBatch{}, ErrTerminalGone
		}
		if after > t.sequence {
			t.mu.Unlock()
			return TerminalBatch{}, errors.New("terminal sequence is in the future")
		}
		if len(t.events) > 0 && after < t.events[0].Sequence-1 {
			t.mu.Unlock()
			return TerminalBatch{}, ErrTerminalOutputGap
		}
		if a.processor {
			if after < t.processed {
				t.mu.Unlock()
				return TerminalBatch{}, errors.New("terminal processor acknowledgement moved backwards")
			}
			if after > t.processed {
				for _, event := range t.events {
					if event.Sequence > t.processed && event.Sequence <= after {
						t.pendingBytes -= len(event.Data)
					}
				}
				t.processed = after
				t.notifyLocked()
			}
		}
		batch := TerminalBatch{Events: make([]TerminalEvent, 0), Sequence: after}
		bytes := 0
		for _, event := range t.events {
			if event.Sequence <= after {
				continue
			}
			// Return independent data: callers cannot mutate retained output.
			copyEvent := event
			copyEvent.Data = append([]byte(nil), event.Data...)
			if event.Size != nil {
				size := *event.Size
				copyEvent.Size = &size
			}
			batch.Events = append(batch.Events, copyEvent)
			batch.Sequence = event.Sequence
			bytes += len(event.Data)
			if bytes >= terminalReadBytes {
				break
			}
		}
		batch.Exited = t.exited && batch.Sequence == t.sequence
		batch.ExitStatus = t.statusLocked()
		changed := t.changed
		t.mu.Unlock()
		if len(batch.Events) > 0 || batch.Exited {
			return batch, nil
		}
		select {
		case <-ctx.Done():
			return TerminalBatch{}, ctx.Err()
		case <-a.detached:
			return TerminalBatch{}, ErrTerminalDetached
		case <-changed:
		}
	}
}

// Write accepts one bounded input frame. One writer drains accepted frames in
// order, including across controller handoff. It never blocks a broker lock on
// an application that has stopped reading stdin.
func (a *TerminalAttachment) Write(data []byte) (int, error) {
	return a.enqueueInput(data, false, nil)
}

// Respond forwards interpreter-generated terminal query responses. Browser/SSH
// observers cannot use it; their input and resize still need the controller.
func (a *TerminalAttachment) Respond(data []byte) (int, error) {
	return a.enqueueInput(data, true, nil)
}

// WriteFrame acknowledges native input consumption. Transports await each frame
// before sending another, so large pastes use bounded flow control. Cancellation
// after admission cannot retract bytes already written and must not retry them.
func (a *TerminalAttachment) WriteFrame(ctx context.Context, data []byte, response bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result := make(chan error, 1)
	if _, err := a.enqueueInput(data, response, result); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-a.detached:
		return ErrTerminalDetached
	case <-a.terminal.done:
		return ErrTerminalGone
	}
}

func (a *TerminalAttachment) enqueueInput(data []byte, response bool, result chan error) (int, error) {
	if len(data) == 0 || len(data) > maxFrame {
		return 0, errors.New("terminal input frame is invalid")
	}
	t := a.terminal
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inputError != nil {
		return 0, t.inputError
	}
	if response {
		if _, ok := t.attachments[a]; !ok {
			return 0, ErrTerminalDetached
		}
		if t.closed || t.exited {
			return 0, ErrTerminalGone
		}
		if !a.processor || t.processor != a {
			return 0, ErrTerminalReadOnly
		}
	} else if err := a.canControlLocked(); err != nil {
		return 0, err
	}
	select {
	case t.input <- terminalInput{data: append([]byte(nil), data...), result: result}:
		return len(data), nil
	default:
		return 0, ErrTerminalInputFull
	}
}

func (a *TerminalAttachment) canControlLocked() error {
	t := a.terminal
	if _, ok := t.attachments[a]; !ok {
		return ErrTerminalDetached
	}
	if t.closed || t.exited {
		return ErrTerminalGone
	}
	if !a.control || t.controller != a {
		return ErrTerminalReadOnly
	}
	return nil
}

func (a *TerminalAttachment) Resize(ctx context.Context, size backend.ConsoleSize) error {
	if size.Columns < 2 || size.Columns > 500 || size.Rows < 1 || size.Rows > 200 {
		return errors.New("console size is outside the supported range")
	}
	t := a.terminal
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := a.canControlLocked(); err != nil {
		return err
	}
	if t.size == size {
		return nil
	}
	if err := t.waitCapacityLocked(ctx, 0); err != nil {
		return err
	}
	if err := a.canControlLocked(); err != nil {
		return err
	}
	// Serialize only this terminal's short ioctl against output publication so
	// the display engine receives new geometry before SIGWINCH-generated bytes.
	if err := t.console.Resize(ctx, size); err != nil {
		return err
	}
	t.size = size
	t.appendLocked(TerminalEvent{Size: &size})
	return nil
}

func (t *Terminal) appendLocked(event TerminalEvent) {
	t.sequence++
	event.Sequence = t.sequence
	t.events = append(t.events, event)
	t.bytes += len(event.Data)
	if t.processor != nil {
		t.pendingBytes += len(event.Data)
	}
	for t.bytes > terminalOutputBytes || len(t.events) > terminalOutputEvents {
		t.bytes -= len(t.events[0].Data)
		t.events[0] = TerminalEvent{}
		t.events = t.events[1:]
	}
	t.notifyLocked()
}

func (t *Terminal) waitCapacityLocked(ctx context.Context, bytes int) error {
	for !t.closed && t.processor != nil &&
		(t.pendingBytes+bytes > terminalOutputBytes || t.sequence-t.processed >= terminalOutputEvents) {
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			t.mu.Lock()
			return ctx.Err()
		case <-changed:
		}
		t.mu.Lock()
	}
	if t.closed {
		return ErrTerminalGone
	}
	return ctx.Err()
}

func (t *Terminal) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *Terminal) readOutput() {
	buffer := make([]byte, 16<<10)
	for {
		n, err := t.console.Read(buffer)
		if n > 0 {
			t.mu.Lock()
			if waitErr := t.waitCapacityLocked(t.manager.lifetime, n); waitErr == nil {
				t.appendLocked(TerminalEvent{Data: append([]byte(nil), buffer[:n]...)})
			} else {
				err = waitErr
			}
			t.mu.Unlock()
		}
		if err != nil {
			t.finish()
			return
		}
	}
}

func (t *Terminal) writeInput() {
	for {
		select {
		case <-t.done:
			return
		case frame := <-t.input:
			data := frame.data
			var failure error
			for len(data) > 0 {
				n, err := t.console.Write(data)
				if err != nil || n == 0 {
					failure = err
					if failure == nil {
						failure = io.ErrNoProgress
					}
					break
				}
				data = data[n:]
			}
			if frame.result != nil {
				frame.result <- failure
			}
			if failure != nil {
				t.mu.Lock()
				t.inputError = failure
				t.mu.Unlock()
				// Reject queued waiters, while output drains the final process bytes.
				for {
					select {
					case pending := <-t.input:
						if pending.result != nil {
							pending.result <- failure
						}
					default:
						return
					}
				}
			}
		}
	}
}

func (t *Terminal) finish() {
	// Read EOF follows the backend's wait/status boundary. Close must release a
	// blocked read/write on explicit destruction, not on transport detach.
	t.mu.Lock()
	if !t.exited {
		t.exited = true
		if value, ok := t.console.(backend.ConsoleExitStatus); ok {
			status := value.ExitStatus()
			t.exitStatus = &status
		}
		close(t.done)
		t.notifyLocked()
	}
	t.mu.Unlock()
	_ = t.console.Close()
	t.cancel()
}

// Close explicitly destroys this terminal and unregisters its identity. Exited
// terminals retain their bounded final output until close or detached expiry.
func (t *Terminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		if t.idleTimer != nil {
			t.idleTimer.Stop()
			t.idleTimer = nil
		}
		for a := range t.attachments {
			a.closeLocked()
		}
		t.events = nil
		t.bytes = 0
		t.notifyLocked()
		t.mu.Unlock()
		t.cancel()
		err = t.console.Close()
		t.finish()
		t.manager.mu.Lock()
		delete(t.manager.terminals, t.id)
		t.manager.mu.Unlock()
		if t.release != nil {
			t.release()
		}
	})
	return err
}

// TerminalClosed distinguishes physical destruction from an attachment loss.
func (a *TerminalAttachment) TerminalClosed() bool {
	a.terminal.mu.Lock()
	defer a.terminal.mu.Unlock()
	return a.terminal.closed
}

// Keep the byte-stream contract explicit: EOF must never be synthesized by a
// detached retained attachment. Connection-bound streams still use OpenConsole.
var _ io.Closer = (*TerminalAttachment)(nil)
