package console

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"the8020/kernel/sandbox/backend"
)

func TestTerminalViewUsesProcessorDisplayAndDetachesWithoutEOF(t *testing.T) {
	m, terminal, opened := newRetainedTerminal(t)
	processor, err := terminal.AttachProcessor(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := m.OpenTerminalView(ctx, terminal.id, "sbx-bbbbbbbbbb", terminal.size); err == nil {
		t.Fatal("accepted mismatched sandbox")
	}
	view, err := m.OpenTerminalView(ctx, terminal.id, "", terminal.size)
	if err != nil {
		t.Fatal(err)
	}
	request, err := processor.NextView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if request.Sequence != 0 || request.Size != terminal.size {
		t.Fatalf("request = %#v", request)
	}
	if _, err := terminal.Attach(true); !errors.Is(err, ErrTerminalBusy) {
		t.Fatalf("second control = %v", err)
	}
	observer, err := terminal.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.WriteView(ctx, request.ViewID, []byte("bad")); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("observer wrote display: %v", err)
	}
	output := []byte("canonical display, without raw query bytes")
	written := make(chan error, 1)
	go func() { written <- processor.WriteView(ctx, request.ViewID, output) }()
	data := make([]byte, len(output))
	if _, err := io.ReadFull(view, data); err != nil || string(data) != string(output) {
		t.Fatalf("display = %q, %v", data, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := view.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened.console.Done():
		t.Fatal("SSH EOF closed native process")
	default:
	}
	if terminal.Info().Exited {
		t.Fatal("detach exited terminal")
	}
	if _, err := view.Write([]byte("stale")); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("stale write = %v", err)
	}
	if err := processor.WriteView(ctx, request.ViewID, []byte("stale")); !errors.Is(err, ErrTerminalDetached) {
		t.Fatalf("stale display = %v", err)
	}
	if _, err := opened.peer.Write([]byte("still draining")); err != nil {
		t.Fatal(err)
	}
	batch, err := processor.Read(ctx, 0)
	if err != nil || string(batch.Events[0].Data) != "still draining" {
		t.Fatalf("detached output = %#v, %v", batch, err)
	}
}

func TestTerminalViewTakeoverAndProcessorLossInterruptBlockedIO(t *testing.T) {
	for _, action := range []string{"takeover", "processor", "close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			m, terminal, opened := newRetainedTerminal(t)
			processor, err := terminal.AttachProcessor(0)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			view, err := m.OpenTerminalView(ctx, terminal.id, "", backend.ConsoleSize{Columns: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			request, err := processor.NextView(ctx)
			if err != nil {
				t.Fatal(err)
			}
			writing := make(chan error, 1)
			go func() { writing <- processor.WriteView(ctx, request.ViewID, []byte("blocked")) }()
			switch action {
			case "takeover":
				if _, err := terminal.TakeControl(); err != nil {
					t.Fatal(err)
				}
			case "processor":
				_ = processor.Close()
			case "close":
				_ = terminal.Close()
			case "cancel":
				cancel()
			}
			select {
			case err := <-writing:
				if err == nil {
					t.Fatal("blocked write succeeded without a reader")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked view writer leaked")
			}
			_ = view.Close()
			if action != "close" {
				select {
				case <-opened.console.Done():
					t.Fatal("view loss killed PTY")
				default:
				}
			}
		})
	}
}
