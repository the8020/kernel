package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"the8020/kernel/console"
	"the8020/kernel/execution"
	"the8020/kernel/identity"
	"the8020/kernel/sandbox/backend"
)

type terminalLease struct {
	terminalID string
	attachment *console.TerminalAttachment
}

// The trusted Worker owns these replaceable interpreter/controller leases. The
// console broker owns the PTYs; releasing a Worker never closes those processes.
type terminalOwner struct {
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	pending     int
	attachments map[string]terminalLease
}

func terminalOwnerKey(sandboxID, workerID string) string { return sandboxID + "\x00" + workerID }

func (d *Dispatcher) ReleaseWorker(sandboxID, workerID string) {
	key := terminalOwnerKey(sandboxID, workerID)
	d.terminalMu.Lock()
	owner := d.terminalOwners[key]
	var leases []terminalLease
	if owner != nil {
		owner.closed = true
		for _, lease := range owner.attachments {
			leases = append(leases, lease)
		}
		clear(owner.attachments)
		if owner.pending == 0 {
			delete(d.terminalOwners, key)
		}
	}
	d.terminalMu.Unlock()
	if owner != nil {
		owner.cancel()
	}
	for _, lease := range leases {
		_ = lease.attachment.Close()
	}
}

func decodeTerminalInput(input map[string]any, value any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func (d *Dispatcher) beginTerminalOwner(ctx context.Context, key string) (context.Context, *terminalOwner, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	d.terminalMu.Lock()
	owner := d.terminalOwners[key]
	if owner == nil {
		lifetime, cancel := context.WithCancel(context.Background())
		owner = &terminalOwner{ctx: lifetime, cancel: cancel, attachments: make(map[string]terminalLease)}
		d.terminalOwners[key] = owner
	}
	if owner.closed {
		d.terminalMu.Unlock()
		return nil, nil, nil, console.ErrTerminalDetached
	}
	owner.pending++
	d.terminalMu.Unlock()
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(owner.ctx, cancel)
	finish := func() {
		stop()
		cancel()
		d.terminalMu.Lock()
		owner.pending--
		if owner.pending == 0 && len(owner.attachments) == 0 && d.terminalOwners[key] == owner {
			delete(d.terminalOwners, key)
			owner.cancel()
		}
		d.terminalMu.Unlock()
	}
	return callCtx, owner, finish, nil
}

func (d *Dispatcher) retainTerminalAttachment(owner *terminalOwner, terminalID string, a *console.TerminalAttachment) error {
	d.terminalMu.Lock()
	defer d.terminalMu.Unlock()
	if owner.closed {
		_ = a.Close()
		return console.ErrTerminalDetached
	}
	owner.attachments[a.ID()] = terminalLease{terminalID: terminalID, attachment: a}
	return nil
}

func (d *Dispatcher) terminal(ctx context.Context, action string, input map[string]any) (any, error) {
	caller, ok := execution.CallerFromContext(ctx)
	if !ok || caller.WorkerID == "" || caller.SandboxID == "" {
		return nil, errors.New("terminal operation requires a trusted Worker")
	}
	m := d.services.PlatformSnapshot().Consoles
	if m == nil {
		return nil, errors.New("terminal broker is unavailable")
	}
	key := terminalOwnerKey(caller.SandboxID, caller.WorkerID)
	switch action {
	case "list":
		var in struct {
			Kind      string `json:"kind"`
			SandboxID string `json:"sandboxId"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		return m.Terminals(in.Kind, in.SandboxID), nil
	case "create":
		var in struct {
			Kind        string              `json:"kind"`
			SandboxID   string              `json:"sandboxId"`
			Arguments   []string            `json:"arguments"`
			Environment []string            `json:"environment"`
			WorkingDir  string              `json:"workingDir"`
			Size        backend.ConsoleSize `json:"size"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		callCtx, owner, finish, err := d.beginTerminalOwner(ctx, key)
		if err != nil {
			return nil, err
		}
		defer finish()
		t, a, err := m.CreateTerminalWithProcessor(callCtx, in.Kind, in.SandboxID, backend.ConsoleOptions{
			Arguments: in.Arguments, Environment: in.Environment, WorkingDir: in.WorkingDir, Size: in.Size, Terminal: true,
		})
		if err != nil {
			return nil, err
		}
		if err := d.retainTerminalAttachment(owner, t.Info().ID, a); err != nil {
			_ = t.Close()
			return nil, err
		}
		return map[string]any{"terminal": t.Info(), "attachmentId": a.ID()}, nil
	case "close":
		var in struct {
			TerminalID string `json:"terminalId"`
			NodeID     string `json:"nodeId,omitempty"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		if in.NodeID != "" {
			nodes := d.services.PlatformSnapshot().Nodes
			if nodes == nil {
				return nil, errors.New("node terminal control is unavailable")
			}
			return nil, nodes.CloseTerminal(ctx, in.NodeID, in.TerminalID)
		}
		return nil, d.CloseTerminal(ctx, in.TerminalID)
	case "inspect":
		var in struct {
			TerminalID string `json:"terminalId"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		t, err := m.Terminal(in.TerminalID)
		if err != nil {
			return nil, err
		}
		return t.Info(), nil
	case "attach":
		var in struct {
			TerminalID string `json:"terminalId"`
			Mode       string `json:"mode"`
			After      uint64 `json:"after"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		t, err := m.Terminal(in.TerminalID)
		if err != nil {
			return nil, err
		}
		_, owner, finish, err := d.beginTerminalOwner(ctx, key)
		if err != nil {
			return nil, err
		}
		defer finish()
		var a *console.TerminalAttachment
		switch in.Mode {
		case "control", "observe":
			a, err = t.Attach(in.Mode == "control")
		case "take-control":
			a, err = t.TakeControl()
		case "process":
			a, err = t.AttachProcessor(in.After)
		default:
			return nil, errors.New("terminal attachment mode must be control, take-control, observe, or process")
		}
		if errors.Is(err, console.ErrTerminalBusy) {
			return map[string]any{"busy": true}, nil
		}
		if err != nil {
			return nil, err
		}
		if err := d.retainTerminalAttachment(owner, in.TerminalID, a); err != nil {
			return nil, err
		}
		return map[string]any{"terminal": t.Info(), "attachmentId": a.ID()}, nil
	case "read", "write", "respond", "resize", "detach", "view-next", "view-write", "view-finish":
		var in struct {
			AttachmentID string              `json:"attachmentId"`
			ViewID       string              `json:"viewId,omitempty"`
			After        uint64              `json:"after,omitempty"`
			Data         []byte              `json:"data,omitempty"`
			Size         backend.ConsoleSize `json:"size,omitempty"`
		}
		if err := decodeTerminalInput(input, &in); err != nil {
			return nil, err
		}
		d.terminalMu.Lock()
		owner := d.terminalOwners[key]
		var a *console.TerminalAttachment
		if owner != nil && !owner.closed {
			a = owner.attachments[in.AttachmentID].attachment
		}
		if a != nil && action == "detach" {
			delete(owner.attachments, in.AttachmentID)
			if len(owner.attachments) == 0 && owner.pending == 0 {
				owner.cancel()
				delete(d.terminalOwners, key)
			}
		}
		d.terminalMu.Unlock()
		if a == nil {
			if action == "detach" {
				return nil, nil
			}
			return nil, console.ErrTerminalDetached
		}
		switch action {
		case "view-next":
			return a.NextView(ctx)
		case "view-write":
			return nil, a.WriteView(ctx, in.ViewID, in.Data)
		case "view-finish":
			return nil, a.FinishView(in.ViewID)
		case "read":
			return a.Read(ctx, in.After)
		case "write":
			return nil, a.WriteFrame(ctx, in.Data, false)
		case "respond":
			// The canonical output reader must keep draining while the process
			// finishes writing output before it reads its query reply.
			_, err := a.Respond(in.Data)
			return nil, err
		case "resize":
			return nil, a.Resize(ctx, in.Size)
		case "detach":
			return nil, a.Close()
		}
	}
	return nil, fmt.Errorf("unknown terminal operation %q", action)
}

// CloseTerminal is shared by local Worker calls and authenticated node control.
// It requires no surviving display Worker and never creates a missing terminal.
func (d *Dispatcher) CloseTerminal(ctx context.Context, terminalID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !identity.Is(terminalID, "tty") {
		return errors.New("canonical terminal ID is required")
	}
	m := d.services.PlatformSnapshot().Consoles
	if m == nil {
		return errors.New("terminal broker is unavailable")
	}
	t, err := m.Terminal(terminalID)
	if errors.Is(err, console.ErrTerminalGone) {
		return nil
	}
	if err != nil {
		return err
	}
	d.terminalMu.Lock()
	for ownerKey, owner := range d.terminalOwners {
		for id, lease := range owner.attachments {
			if lease.terminalID == terminalID {
				_ = lease.attachment.Close()
				delete(owner.attachments, id)
			}
		}
		if owner.pending == 0 && len(owner.attachments) == 0 {
			owner.cancel()
			delete(d.terminalOwners, ownerKey)
		}
	}
	d.terminalMu.Unlock()
	return t.Close()
}
