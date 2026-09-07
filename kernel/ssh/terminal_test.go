package sshserver

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"the8020/kernel/console"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
)

type retainedProvider struct {
	process *fakeConsole
	opens   int
}

func (p *retainedProvider) HasSandbox(id string) bool { return id == "sbx-aaaaaaaaaa" }
func (p *retainedProvider) OpenConsole(context.Context, string, backend.ConsoleOptions) (backend.Console, error) {
	p.opens++
	return p.process, nil
}

type retainedAuth struct{}

func (retainedAuth) AuthenticateToken(context.Context, string) (execution.User, error) {
	return execution.User{ID: "user:alice", Username: "alice"}, nil
}

func TestSSHRetainedTerminalSelectionAndDetach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	provider := &retainedProvider{process: newFakeConsole()}
	broker, err := console.New(console.Config{Authentication: retainedAuth{}, Development: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	options := backend.ConsoleOptions{Arguments: []string{"/bin/bash"}, WorkingDir: "/workspace", Size: backend.ConsoleSize{Columns: 80, Rows: 24}, Terminal: true}
	terminal, processor, err := broker.CreateTerminalWithProcessor(ctx, "development", "sbx-aaaaaaaaaa", options)
	if err != nil {
		t.Fatal(err)
	}
	development := &fakeDevelopment{sandbox: "sbx-bbbbbbbbbb"}
	manager, err := New(Config{Port: 0, HostKeyPath: filepath.Join(t.TempDir(), "host"), Authentication: testAuthentication(), Development: development, Consoles: broker})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	client, err := gossh.Dial("tcp", "127.0.0.1:"+stringPort(manager.Port()), clientConfig("alice", "correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	selector := "the8020 terminal-id=" + terminal.Info().ID
	for _, attempt := range []struct {
		command string
		pty     bool
	}{
		{selector, false},
		{selector + " sandbox-id=sbx-bbbbbbbbbb", true},
		{"the8020 terminal-id=tty-aaaaaaaaaa", true},
	} {
		s, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if attempt.pty {
			if err := s.RequestPty("xterm", 24, 80, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Start(attempt.command); err == nil {
			t.Fatal("invalid attachment accepted")
		}
		_ = s.Close()
	}
	for i := 0; i < 2; i++ {
		s, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		stdin, _ := s.StdinPipe()
		stdout, _ := s.StdoutPipe()
		if err := s.RequestPty("xterm-256color", 24, 80, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(selector + " sandbox-id=sbx-aaaaaaaaaa"); err != nil {
			t.Fatal(err)
		}
		request, err := processor.NextView(ctx)
		if err != nil {
			t.Fatal(err)
		}
		written := make(chan error, 1)
		go func() { written <- processor.WriteView(ctx, request.ViewID, []byte("restored screen")) }()
		data := make([]byte, len("restored screen"))
		if _, err := io.ReadFull(stdout, data); err != nil || string(data) != "restored screen" {
			t.Fatalf("display = %q, %v", data, err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write([]byte("\x1b[A\x03")); err != nil {
			t.Fatal(err)
		}
		if got := receiveBytes(t, provider.process.input); string(got) != "\x1b[A\x03" {
			t.Fatalf("input = %q", got)
		}
		_ = stdin.Close()
		_ = s.Wait()
		_ = s.Close()
		select {
		case <-provider.process.done:
			t.Fatal("SSH EOF killed retained process")
		default:
		}
		select {
		case got := <-provider.process.input:
			t.Fatalf("implicit input on detach: %q", got)
		default:
		}
	}
	if provider.opens != 1 || len(development.users) != 0 {
		t.Fatalf("attachment spawned a process or ensured default sandbox: %d, %v", provider.opens, development.users)
	}
}

func TestTerminalSelectorGrammar(t *testing.T) {
	for _, command := range []string{"the8020 terminal-id=tty-aaaaaaaaaa", "the8020 sandbox-id=sbx-aaaaaaaaaa terminal-id=tty-bbbbbbbbbb"} {
		got, err := parseExec(command)
		if err != nil || got.terminalID == "" || got.command != "" {
			t.Fatalf("selector %q = %#v, %v", command, got, err)
		}
	}
	for _, command := range []string{"the8020 terminal-id=sbx-aaaaaaaaaa", "the8020 terminal-id=tty-aaaaaaaaaa terminal-id=tty-aaaaaaaaaa", "the8020 terminal-id=", "the8020 terminal-id=tty-aaaaaaaaaa extra"} {
		if _, err := parseExec(command); err == nil {
			t.Fatalf("accepted %q", command)
		}
	}
}
