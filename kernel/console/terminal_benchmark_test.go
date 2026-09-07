package console

import (
	"context"
	"io"
	"testing"
)

// These isolate broker cost using the same native-stream fixture on both paths.
// They exclude runsc, WebSockets, Deno parsing, and browser rendering.
func BenchmarkConsoleOutput(b *testing.B) {
	for _, retained := range []bool{false, true} {
		name := "connection-bound"
		if retained {
			name = "retained-detached"
		}
		b.Run(name, func(b *testing.B) {
			provider := &testProvider{opened: make(chan testOpen, 1)}
			manager, err := New(Config{Authentication: testAuthentication{}, Development: provider})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = manager.Close() })
			if retained {
				if _, err := manager.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions()); err != nil {
					b.Fatal(err)
				}
			} else {
				lease, err := manager.OpenConsole(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
				if err != nil {
					b.Fatal(err)
				}
				go func() { _, _ = io.CopyBuffer(discardOutput{}, lease, make([]byte, 16<<10)) }()
			}
			opened := <-provider.opened
			b.Cleanup(func() { _ = opened.peer.Close() })
			chunk := make([]byte, 16<<10)
			b.SetBytes(int64(len(chunk)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := opened.peer.Write(chunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type discardOutput struct{}

func (discardOutput) Write(p []byte) (int, error) { return len(p), nil }

func BenchmarkTerminalRead(b *testing.B) {
	provider := &testProvider{opened: make(chan testOpen, 1)}
	manager, err := New(Config{Authentication: testAuthentication{}, Development: provider})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = manager.Close() })
	terminal, err := manager.CreateTerminal(context.Background(), "development", "sbx-aaaaaaaaaa", retainedOptions())
	if err != nil {
		b.Fatal(err)
	}
	opened := <-provider.opened
	b.Cleanup(func() { _ = opened.peer.Close() })
	a, err := terminal.Attach(false)
	if err != nil {
		b.Fatal(err)
	}
	chunk := make([]byte, 16<<10)
	// Wait for each observer acknowledgement: this measures delivered bytes,
	// including ownership handoff, rather than a faster producer dropping data.
	delivered := make(chan error)
	go func() {
		var sequence uint64
		for {
			batch, err := a.Read(context.Background(), sequence)
			if err != nil {
				return
			}
			sequence = batch.Sequence
			delivered <- nil
		}
	}()
	b.Cleanup(func() { _ = a.Close() })
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := opened.peer.Write(chunk); err != nil {
			b.Fatal(err)
		}
		<-delivered
	}
}
