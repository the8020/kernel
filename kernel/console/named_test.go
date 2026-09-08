package console

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNamedTerminalOpenReusesRecreatesAndClaimsLostProcessor(t *testing.T) {
	provider := &testProvider{opened: make(chan testOpen, 8)}
	m, err := New(Config{Authentication: testAuthentication{}, Development: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer func() {
		for len(provider.opened) > 0 {
			_ = (<-provider.opened).peer.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	owner := TerminalOwner{NodeID: "nod-aaaaaaaaaa", SandboxID: "sbx-bbbbbbbbbb", WorkerID: "wrk-aaaaaaaaaa", PersistentExecutionID: "pex-aaaaaaaaaa"}
	open := func(sandbox, name string) OpenedTerminal {
		t.Helper()
		got, err := m.OpenTerminal(ctx, "development", sandbox, name, retainedOptions(), owner)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	const sandbox = "sbx-aaaaaaaaaa"
	var wg sync.WaitGroup
	results := make(chan OpenedTerminal, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.OpenTerminal(ctx, "development", sandbox, "abc_1-test", retainedOptions(), owner)
			if err != nil {
				t.Error(err)
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	var first OpenedTerminal
	var reused []*Terminal
	for got := range results {
		if got.Created {
			if first.Created {
				t.Fatal("competing opens created two processors")
			}
			first = got
		} else {
			if got.Attachment != nil || got.Owner != owner {
				t.Fatalf("reuse = %#v", got)
			}
			reused = append(reused, got.Terminal)
		}
	}
	if !first.Created || first.Attachment == nil || first.Terminal.Info().SessionID != "abc_1-test" || len(reused) != 15 {
		t.Fatalf("creation = %#v; reused %d", first, len(reused))
	}
	for _, terminal := range reused {
		if terminal != first.Terminal {
			t.Fatal("competing opens returned different processes")
		}
	}
	if len(provider.opened) != 1 {
		t.Fatal("parallel opens created extra shells")
	}
	if other := open("sbx-bbbbbbbbbb", "abc_1-test"); other.Terminal == first.Terminal {
		t.Fatal("session name crossed sandbox boundary")
	}
	_ = first.Attachment.Close()
	adopted := open(sandbox, "abc_1-test")
	if !adopted.Reset || adopted.Created || adopted.Attachment == nil || adopted.Terminal != first.Terminal {
		t.Fatalf("adoption = %#v", adopted)
	}
	_ = first.Terminal.Close()
	replacement := open(sandbox, "abc_1-test")
	if !replacement.Created || replacement.Terminal.Info().ID == first.Terminal.Info().ID {
		t.Fatal("closed shell was not recreated with a new physical identity")
	}
	_ = replacement.Terminal.console.Close()
	<-replacement.Terminal.done
	if exited := open(sandbox, "abc_1-test"); !exited.Created || exited.Terminal == replacement.Terminal {
		t.Fatal("exited shell was not recreated")
	}
	if !open(sandbox, strings.Repeat("a", 40)).Created {
		t.Fatal("40 character ID rejected")
	}
	for _, name := range []string{"", strings.Repeat("a", 41), "a b", "a/b", "a.b", "é", "a\n"} {
		if _, err := m.OpenTerminal(ctx, "development", sandbox, name, retainedOptions(), owner); err == nil {
			t.Errorf("invalid ID accepted: %q", name)
		}
	}
}
