package development

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	platformconsole "the8020/kernel/console"
	"the8020/kernel/sandbox/backend"
)

// This exercises real native PTYs, without a terminal multiplexer. Browser and
// agent display qualification is separate and cannot be inferred from PID tests.
func TestRootlessRetainedTerminals(t *testing.T) {
	if os.Getenv("THE8020_TERMINAL_E2E") != "1" {
		t.Skip("set THE8020_TERMINAL_E2E=1 after portable runtime installation")
	}
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	if err := os.MkdirAll(packages, 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyDirectory(context.Background(), filepath.Join(source, "defaults/scripts"), filepath.Join(root, "scripts")); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "node/kernel/runtime/development")
	driver := NewRootlessDriver(RootlessConfig{
		RunscPath:   filepath.Join(source, ".development/runtime/gvisor/bin/runsc"),
		RuntimeRoot: filepath.Join(runtimeRoot, "runsc"), SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"),
	})
	manager, err := New(Config{
		Root: root, PackagesRoot: packages, UsersRoot: filepath.Join(root, "users"), RuntimeRoot: runtimeRoot,
		ImageRoot:   filepath.Join(source, ".development/runtime/development/rootfs"),
		ImageRecord: filepath.Join(source, ".development/runtime/development/image.json"),
		Driver:      driver, ActivationGateway: NewCommandBusGateway(core.NewRegistry(nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	sandbox, err := manager.Create(context.Background(), "terminalproof")
	if err != nil {
		t.Fatal(err)
	}
	broker, err := platformconsole.New(platformconsole.Config{Authentication: sshProofAuthentication{}, Development: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	terminals := make([]*platformconsole.Terminal, 2)
	sequences := make([]uint64, 2)
	for i := range terminals {
		ctx, cancel := context.WithCancel(context.Background())
		terminal, err := broker.CreateTerminal(ctx, "development", sandbox.SandboxID, backend.ConsoleOptions{
			Arguments: []string{"/bin/bash", "-l"}, WorkingDir: "/workspace", Terminal: true,
			Environment: []string{"TERM=xterm-256color", "HOME=/root", "PATH=" + developmentPath}, Size: backend.ConsoleSize{Columns: 90, Rows: 27},
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		terminals[i] = terminal
		a, err := terminal.Attach(true)
		if err != nil {
			t.Fatal(err)
		}
		command := fmt.Sprintf("stty -echo; export PS1=''; printf '%%s' $$ > /tmp/terminal-%d.pid; printf 'READY-%%s\\n' %d\n", i, i)
		if _, err := a.Write([]byte(command)); err != nil {
			t.Fatal(err)
		}
		sequences[i] = retainedUntil(t, a, 0, fmt.Sprintf("READY-%d", i))
		_ = a.Close()
	}
	pidBefore := retainedExec(t, driver, sandbox.SandboxID, "cat /tmp/terminal-0.pid; echo; cat /tmp/terminal-1.pid")
	for cycle := 0; cycle < 20; cycle++ {
		i := cycle % 2
		a, err := terminals[i].Attach(true)
		if err != nil {
			t.Fatal(err)
		}
		marker := fmt.Sprintf("cycle-%02d", cycle)
		if _, err := a.Write([]byte(fmt.Sprintf("printf '%%s\\n' %s\n", marker))); err != nil {
			t.Fatal(err)
		}
		sequences[i] = retainedUntil(t, a, sequences[i], marker)
		_ = a.Close()
	}
	pidAfter := retainedExec(t, driver, sandbox.SandboxID, "cat /tmp/terminal-0.pid; echo; cat /tmp/terminal-1.pid; kill -0 $(cat /tmp/terminal-0.pid) $(cat /tmp/terminal-1.pid)")
	if pidBefore != pidAfter {
		t.Fatalf("terminal processes changed: %q -> %q", pidBefore, pidAfter)
	}
	t.Logf("two native shell PIDs retained through 20 detach/switch/attach cycles: %q", pidAfter)

	a, err := terminals[0].Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("head -c 4194304 /dev/zero | tr '\\0' x; printf done > /tmp/detached-output-done\n")); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	retainedExec(t, driver, sandbox.SandboxID, "for i in $(seq 1 100); do test -f /tmp/detached-output-done && exit 0; sleep .05; done; exit 1")
	t.Log("4 MiB detached native PTY output completed without a reader attachment")
	if err := terminals[0].Close(); err != nil {
		t.Fatal(err)
	}
	retainedExec(t, driver, sandbox.SandboxID, "for i in $(seq 1 100); do kill -0 $(cat /tmp/terminal-0.pid) 2>/dev/null || exit 0; sleep .05; done; exit 1")
	a, err = terminals[1].Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("printf 'FINAL-output\\n'; exit 7\n")); err != nil {
		t.Fatal(err)
	}
	// The raw runsc PTY reports EOF but does not expose a process exit code.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := ""
	for {
		batch, err := a.Read(ctx, sequences[1])
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range batch.Events {
			output += string(event.Data)
		}
		sequences[1] = batch.Sequence
		if batch.Exited {
			break
		}
	}
	if !strings.Contains(output, "FINAL-output") {
		t.Fatalf("final output lost: %q", output)
	}
	t.Log("explicit close ended only its PTY; the other shell exited independently with its final output retained")
}

func retainedUntil(t *testing.T, a *platformconsole.TerminalAttachment, after uint64, marker string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := ""
	for {
		batch, err := a.Read(ctx, after)
		if err != nil {
			t.Fatalf("wait for %q: %v; output %q", marker, err, output)
		}
		for _, event := range batch.Events {
			output += string(event.Data)
		}
		after = batch.Sequence
		if strings.Contains(output, marker) {
			return after
		}
		if batch.Exited {
			t.Fatalf("terminal exited before %q: %q", marker, output)
		}
	}
}

func retainedExec(t *testing.T, driver *RunscDriver, sandboxID, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := driver.Exec(ctx, sandboxID, command)
	if err != nil {
		t.Fatalf("native terminal check: %v: %s", err, output)
	}
	return string(output)
}
