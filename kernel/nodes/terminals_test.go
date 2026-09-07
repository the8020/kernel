package nodes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type terminalCloserFunc func(context.Context, string) error

func (f terminalCloserFunc) CloseTerminal(ctx context.Context, id string) error { return f(ctx, id) }

func TestTerminalCloseUsesExactAuthenticatedNodeWithoutWorker(t *testing.T) {
	owner, peer, server := logPeers(t, nil)
	var calls atomic.Int32
	owner.SetTerminalCloser(terminalCloserFunc(func(_ context.Context, id string) error {
		if id != "tty-aaaaaaaaaa" {
			t.Errorf("terminal=%q", id)
		}
		calls.Add(1)
		return nil
	}))
	peer.SetTerminalCloser(terminalCloserFunc(func(context.Context, string) error {
		t.Error("remote close fell back to the receiving node")
		return nil
	}))
	for i := 0; i < 2; i++ {
		if err := peer.CloseTerminal(context.Background(), owner.localID, "tty-aaaaaaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("close calls=%d", calls.Load())
	}
	for _, item := range []struct {
		body, auth string
		status     int
	}{
		{`{"node_id":"nod-aaaaaaaaaa","terminal_id":"tty-aaaaaaaaaa"}`, "", http.StatusUnauthorized},
		{`{"node_id":"nod-bbbbbbbbbb","terminal_id":"tty-aaaaaaaaaa"}`, testSharedSecret, http.StatusBadRequest},
		{`{"node_id":"nod-aaaaaaaaaa","terminal_id":"invalid"}`, testSharedSecret, http.StatusBadRequest},
		{`{"node_id":"nod-aaaaaaaaaa","terminal_id":"tty-aaaaaaaaaa","extra":true}`, testSharedSecret, http.StatusBadRequest},
		{strings.Repeat(" ", terminalControlBytes+1), testSharedSecret, http.StatusBadRequest},
	} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+terminalClosePath, strings.NewReader(item.body))
		if item.auth != "" {
			request.Header.Set("Authorization", "Bearer "+item.auth)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != item.status {
			t.Fatalf("status=%d, want %d", response.StatusCode, item.status)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("invalid request closed a terminal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := peer.CloseTerminal(ctx, owner.localID, "tty-aaaaaaaaaa"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close=%v", err)
	}
	server.Close()
	if err := peer.CloseTerminal(context.Background(), owner.localID, "tty-aaaaaaaaaa"); err == nil {
		t.Fatal("unavailable node reported success")
	}
}

func TestTerminalCloseRequiresBoundedMatchingAcknowledgement(t *testing.T) {
	for _, body := range []string{
		`{"node_id":"nod-bbbbbbbbbb","terminal_id":"tty-aaaaaaaaaa","closed":true}`,
		`{"node_id":"nod-aaaaaaaaaa","terminal_id":"tty-bbbbbbbbbb","closed":true}`,
		`{"node_id":"nod-aaaaaaaaaa","terminal_id":"tty-aaaaaaaaaa","closed":false}`,
		`{"node_id":"nod-aaaaaaaaaa","terminal_id":"tty-aaaaaaaaaa","closed":true} {}`,
		strings.Repeat(" ", terminalControlBytes+1),
	} {
		owner, peer, _ := logPeers(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		if err := peer.CloseTerminal(context.Background(), owner.localID, "tty-aaaaaaaaaa"); err == nil {
			t.Fatal("invalid acknowledgement reported success")
		}
	}
}
