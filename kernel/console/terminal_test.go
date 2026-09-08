package console

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/sandbox/backend"
)

func newRetainedTerminal(t *testing.T) (*Manager, *Terminal, testOpen) {
	t.Helper()
	provider := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: provider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	terminal, err := manager.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
	if err != nil {
		t.Fatal(err)
	}
	opened := <-provider.opened
	t.Cleanup(func() { _ = opened.peer.Close() })
	return manager, terminal, opened
}

func retainedOptions() backend.ConsoleOptions {
	return backend.ConsoleOptions{Arguments: []string{"/bin/bash", "-l"}, WorkingDir: "/workspace",
		Environment: []string{"TERM=xterm-256color"}, Size: backend.ConsoleSize{Columns: 80, Rows: 24}, Terminal: true}
}

func readTerminal(t *testing.T, a *TerminalAttachment, after uint64) TerminalBatch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := a.Read(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRetainedTerminalDetachPreservesProcessAndDrainsOutput(t *testing.T) {
	manager, terminal, opened := newRetainedTerminal(t)
	attachment, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() { _, err := attachment.Read(context.Background(), 0); waiting <- err }()
	if err := attachment.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waiting:
		if !errors.Is(err, ErrTerminalDetached) {
			t.Fatalf("detached read = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detach did not interrupt its reader")
	}
	select {
	case <-opened.console.Done():
		t.Fatal("detach closed the PTY")
	default:
	}
	_ = opened.peer.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := opened.peer.Write([]byte("detached output")); err != nil {
		t.Fatal(err)
	}
	reattached, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	batch := readTerminal(t, reattached, 0)
	if len(batch.Events) != 1 || string(batch.Events[0].Data) != "detached output" || batch.Sequence != 1 {
		t.Fatalf("reattached output = %#v", batch)
	}
	resolved, err := manager.Terminal(terminal.Info().ID)
	if err != nil || resolved != terminal {
		t.Fatalf("reattachment replaced owner: %v", err)
	}
	if got := manager.Terminals("development", "sbx-aaaaaaaaaa"); len(got) != 1 || got[0].Exited {
		t.Fatalf("catalog = %#v", got)
	}
	if _, err := attachment.Write([]byte("stale")); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("detached input = %v", err)
	}
	if _, err := reattached.Write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	_ = opened.peer.SetReadDeadline(time.Now().Add(time.Second))
	data := make([]byte, 4)
	if _, err := io.ReadFull(opened.peer, data); err != nil || string(data) != "live" {
		t.Fatalf("input = %q, %v", data, err)
	}
}

func TestRetainedTerminalHasExclusiveControllerAndOrderedResize(t *testing.T) {
	_, terminal, opened := newRetainedTerminal(t)
	controller, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Attach(true); !errors.Is(err, ErrTerminalBusy) {
		t.Fatalf("second controller = %v", err)
	}
	observer, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Write([]byte("bad")); !errors.Is(err, ErrTerminalReadOnly) {
		t.Fatalf("observer input = %v", err)
	}
	size := backend.ConsoleSize{Columns: 100, Rows: 31}
	if err := observer.Resize(context.Background(), size); !errors.Is(err, ErrTerminalReadOnly) {
		t.Fatalf("observer resize = %v", err)
	}
	if err := controller.Resize(context.Background(), size); err != nil {
		t.Fatal(err)
	}
	if got := <-opened.console.resized; got != size {
		t.Fatalf("PTY size = %#v", got)
	}
	if _, err := opened.peer.Write([]byte("after resize")); err != nil {
		t.Fatal(err)
	}
	batch := readTerminal(t, observer, 0)
	if batch.Events[0].Size == nil || *batch.Events[0].Size != size {
		t.Fatalf("resize event = %#v", batch)
	}
	if len(batch.Events) == 1 {
		batch = readTerminal(t, observer, batch.Sequence)
	}
	last := batch.Events[len(batch.Events)-1]
	if last.Sequence != 2 || string(last.Data) != "after resize" {
		t.Fatalf("output order = %#v", batch)
	}
	_ = controller.Close()
	replacement, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := controller.Resize(context.Background(), size); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("stale resize = %v", err)
	}
}

func TestRetainedTerminalInputAcknowledgementWaitsForNativeConsumption(t *testing.T) {
	_, terminal, opened := newRetainedTerminal(t)
	a, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.WriteFrame(ctx, []byte("a complete paste"), false) }()
	// net.Pipe cannot complete the write until every byte is consumed.
	first := make([]byte, 1)
	if _, err := io.ReadFull(opened.peer, first); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("input acknowledged before consumption: %v", err)
	default:
	}
	rest := make([]byte, len("a complete paste")-1)
	if _, err := io.ReadFull(opened.peer, rest); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(first)+string(rest) != "a complete paste" {
		t.Fatal("paste was altered")
	}
	go func() { done <- a.WriteFrame(ctx, []byte("blocked"), false) }()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrTerminalDetached) {
			t.Fatalf("detached input waiter=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("detach did not release input waiter")
	}
}

func TestRetainedTerminalSlowReaderCannotBlockOutputOrHideGap(t *testing.T) {
	_, terminal, opened := newRetainedTerminal(t)
	observer, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.peer.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.CopyN(opened.peer, infiniteByteReader{}, 4*terminalOutputBytes); err != nil {
		t.Fatalf("detached drain blocked: %v", err)
	}
	if _, err := observer.Read(context.Background(), 0); !errors.Is(err, ErrTerminalOutputGap) {
		t.Fatalf("lagging reader = %v", err)
	}
	terminal.mu.Lock()
	retainedBytes, retainedEvents := terminal.bytes, len(terminal.events)
	// The final native read may still be publishing; inspect a tail frame that
	// cannot be evicted by that last read.
	after := terminal.events[len(terminal.events)-1].Sequence - 1
	terminal.mu.Unlock()
	if retainedBytes > terminalOutputBytes || retainedEvents > terminalOutputEvents {
		t.Fatalf("unbounded output: %d / %d", retainedBytes, retainedEvents)
	}
	batch := readTerminal(t, observer, after)
	if len(batch.Events) == 0 {
		t.Fatal("retained tail unavailable")
	}
	batch.Events[0].Data[0] = 0
	again := readTerminal(t, observer, after)
	if again.Events[0].Data[0] != 'x' {
		t.Fatal("consumer mutated retained output")
	}
}

type infiniteByteReader struct{}

func (infiniteByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestRetainedTerminalBlockedInputIsBoundedAndCloseReleasesIt(t *testing.T) {
	manager, terminal, _ := newRetainedTerminal(t)
	controller, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	full := false
	for i := 0; i < terminalInputFrames+2; i++ {
		_, err := controller.Write(bytes.Repeat([]byte{'x'}, maxFrame))
		if errors.Is(err, ErrTerminalInputFull) {
			full = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !full {
		t.Fatal("stalled stdin accepted unbounded input")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { _ = manager.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stalled stdin blocked broker shutdown")
	}
}

func TestRetainedTerminalExitRetainsFinalBytesWithoutRespawn(t *testing.T) {
	manager, terminal, opened := newRetainedTerminal(t)
	if _, err := opened.peer.Write([]byte("final output")); err != nil {
		t.Fatal(err)
	}
	_ = opened.peer.Close()
	observer, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	batch := readTerminal(t, observer, 0)
	if !batch.Exited {
		batch = readTerminal(t, observer, batch.Sequence)
	}
	if !batch.Exited || batch.ExitStatus == nil || *batch.ExitStatus != 19 {
		t.Fatalf("process result = %#v", batch)
	}
	if !terminal.Info().Exited {
		t.Fatal("process exit not reflected in catalog")
	}
	if _, err := terminal.Attach(false); err != nil {
		t.Fatal(err)
	}
	id := terminal.Info().ID
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Terminal(id); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("closed identity = %v", err)
	}
	if got := manager.Terminals("development", "sbx-aaaaaaaaaa"); len(got) != 0 {
		t.Fatalf("closed catalog = %#v", got)
	}
}

type cancellationProvider struct {
	*testProvider
	ctx chan context.Context
}

func (p *cancellationProvider) OpenConsole(ctx context.Context, id string, options backend.ConsoleOptions) (backend.Console, error) {
	p.ctx <- ctx
	return p.testProvider.OpenConsole(ctx, id, options)
}

func TestRetainedTerminalOutlivesCreatingRequest(t *testing.T) {
	provider := &cancellationProvider{&testProvider{opened: make(chan testOpen, 1)}, make(chan context.Context, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: provider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	terminal, err := manager.CreateTerminal(ctx, "development", "sbx-aaaaaaaaaa", retainedOptions())
	if err != nil {
		t.Fatal(err)
	}
	opened := <-provider.opened
	t.Cleanup(func() { _ = opened.peer.Close() })
	ownerCtx := <-provider.ctx
	cancel()
	if err := ownerCtx.Err(); err != nil {
		t.Fatalf("request ended owner context: %v", err)
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ownerCtx.Err(), context.Canceled) {
		t.Fatal("explicit close did not cancel owner context")
	}
}

func TestRetainedTerminalProviderReplacementClosesOldOwner(t *testing.T) {
	manager, _, _ := newRetainedTerminal(t)
	provider := &testProvider{opened: make(chan testOpen, 1)}
	manager.SetRuntime(provider)
	terminal, err := manager.CreateTerminal(context.Background(), "runtime", "sbx-bbbbbbbbbb", retainedOptions())
	if err != nil {
		t.Fatal(err)
	}
	opened := <-provider.opened
	t.Cleanup(func() { _ = opened.peer.Close() })
	manager.SetRuntime(&testProvider{opened: make(chan testOpen, 1)})
	select {
	case <-opened.console.Done():
	case <-time.After(time.Second):
		t.Fatal("old provider's PTY remained alive")
	}
	if _, err := manager.Terminal(terminal.Info().ID); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("old provider identity = %v", err)
	}
}

func TestRetainedTerminalConcurrentControlHasOneWinner(t *testing.T) {
	_, terminal, _ := newRetainedTerminal(t)
	var wg sync.WaitGroup
	winners := make(chan *TerminalAttachment, 32)
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			a, err := terminal.Attach(true)
			if err == nil {
				winners <- a
			} else if !errors.Is(err, ErrTerminalBusy) {
				t.Errorf("acquire = %v", err)
			}
		})
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("controllers = %d", len(winners))
	}
	for a := range winners {
		_ = a.Close()
	}
}

type blockedOpenProvider struct {
	entered chan struct{}
	count   atomic.Int32
}

func (*blockedOpenProvider) HasSandbox(string) bool { return true }
func (p *blockedOpenProvider) OpenConsole(ctx context.Context, _ string, _ backend.ConsoleOptions) (backend.Console, error) {
	p.count.Add(1)
	p.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestConsoleCreationBoundsPendingNativeProcessesAndCancelsShutdown(t *testing.T) {
	p := &blockedOpenProvider{entered: make(chan struct{}, maxSessions)}
	m, err := New(Config{Authentication: testAuthentication{}, Development: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	results := make(chan error, maxSessions)
	for i := 0; i < maxSessions; i++ {
		go func(retained bool) {
			var err error
			if retained {
				_, err = m.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
			} else {
				_, err = m.OpenConsole(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
			}
			results <- err
		}(i%2 == 0)
	}
	for i := 0; i < maxSessions; i++ {
		select {
		case <-p.entered:
		case <-time.After(time.Second):
			t.Fatal("opening did not enter provider")
		}
	}
	if _, err := m.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions()); err == nil {
		t.Fatal("unbounded retained opening")
	}
	if _, err := m.OpenConsole(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions()); err == nil {
		t.Fatal("unbounded direct opening")
	}
	if p.count.Load() != maxSessions {
		t.Fatalf("native openings = %d", p.count.Load())
	}
	_ = m.Close()
	for i := 0; i < maxSessions; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel opening = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("broker shutdown left an opening blocked")
		}
	}
}

func TestTerminalProcessorIsLosslessAndIndependentOfViews(t *testing.T) {
	provider := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: provider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	terminal, processor, err := manager.CreateTerminalWithProcessor(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
	if err != nil {
		t.Fatal(err)
	}
	opened := <-provider.opened
	t.Cleanup(func() { _ = opened.peer.Close() })
	if _, err := terminal.AttachProcessor(0); !errors.Is(err, ErrTerminalProcessorBusy) {
		t.Fatalf("second processor = %v", err)
	}
	view, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Respond([]byte("fake response")); !errors.Is(err, ErrTerminalReadOnly) {
		t.Fatalf("view response = %v", err)
	}
	if _, err := processor.Write([]byte("fake input")); !errors.Is(err, ErrTerminalReadOnly) {
		t.Fatalf("processor user input = %v", err)
	}

	payload := make([]byte, 3*terminalOutputBytes)
	for i := range payload {
		payload[i] = byte(i ^ (i >> 12) ^ (i >> 18))
	}
	written := make(chan error, 1)
	go func() { _, err := opened.peer.Write(payload); written <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		terminal.mu.Lock()
		pending, changed := terminal.pendingBytes, terminal.changed
		terminal.mu.Unlock()
		if pending == terminalOutputBytes {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("processor window never filled")
		}
	}
	// An unconsumed processor window is bounded. A browser read does not release
	// it, and detaching that browser does not detach the state owner.
	_ = readTerminal(t, view, 0)
	_ = view.Close()
	select {
	case err := <-written:
		t.Fatalf("unacknowledged output was dropped: %v", err)
	default:
	}
	if _, err := processor.Respond([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	_ = opened.peer.SetReadDeadline(time.Now().Add(time.Second))
	reply := make([]byte, 5)
	if _, err := io.ReadFull(opened.peer, reply); err != nil || string(reply) != "reply" {
		t.Fatalf("detached query reply = %q: %v", reply, err)
	}
	var after uint64
	var received []byte
	for len(received) < len(payload) {
		batch, err := processor.Read(ctx, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range batch.Events {
			if event.Sequence != after+1 {
				t.Fatalf("processor sequence gap: %d -> %d", after, event.Sequence)
			}
			after = event.Sequence
			received = append(received, event.Data...)
		}
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("lossless processor changed output")
	}
	if _, err := terminal.AttachProcessor(0); !errors.Is(err, ErrTerminalProcessorBusy) {
		t.Fatalf("owner unexpectedly released = %v", err)
	}
	lagging, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lagging.Read(ctx, 0); !errors.Is(err, ErrTerminalOutputGap) {
		t.Fatalf("slow view = %v", err)
	}
}

func TestTerminalProcessorLossDoesNotTerminatePTYOrBlockShutdown(t *testing.T) {
	_, terminal, opened := newRetainedTerminal(t)
	processor, err := terminal.AttachProcessor(0)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { _, err := opened.peer.Write(make([]byte, 2*terminalOutputBytes)); written <- err }()
	_ = processor.Close()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lost state owner blocked detached PTY draining")
	}
	if _, err := terminal.AttachProcessor(0); !errors.Is(err, ErrTerminalOutputGap) {
		t.Fatalf("lost history was hidden: %v", err)
	}
	select {
	case <-opened.console.Done():
		t.Fatal("state owner loss closed the PTY")
	default:
	}
	if _, err := processor.Respond([]byte("stale")); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("stale processor reply = %v", err)
	}
	_ = terminal.Close()
}
