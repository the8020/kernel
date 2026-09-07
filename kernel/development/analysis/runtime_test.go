//go:build workflowanalysis

// This file is compiled into the development package by run.py's Go overlay.
// It characterizes production and tests prototypes; it is not a release gate.
package development

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"the8020/kernel/cbus/core"
	platformconsole "the8020/kernel/console"
	"the8020/kernel/execution"
	"the8020/kernel/sandbox/backend"
)

type analysisAuthentication struct{ loggedIn atomic.Bool }

func (a *analysisAuthentication) AuthenticateToken(_ context.Context, token string) (execution.User, error) {
	if !a.loggedIn.Load() || token != "analysis" {
		return execution.User{}, errors.New("unauthenticated")
	}
	return execution.UserForUsername("analysisa")
}

func analysisRuntime(t *testing.T) (*Manager, *RunscDriver, string) {
	t.Helper()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	for _, directory := range []string{packages} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyDirectory(context.Background(), filepath.Join(source, "defaults/scripts"), filepath.Join(root, "scripts")); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(packages, "the8020/dev-core")
	writeTestFile(t, filepath.Join(repository, "package.toml"), "schema = 1\n")
	writeTestFile(t, filepath.Join(repository, ".gitignore"), "ignored/\n")
	writeTestFile(t, filepath.Join(repository, "same.txt"), "base\n")
	writeTestFile(t, filepath.Join(repository, "disjoint.txt"), "first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(repository, "untouched.txt"), "before\n")
	runtimeRoot := filepath.Join(root, "node/kernel/runtime/development")
	driver := NewRootlessDriver(RootlessConfig{
		RunscPath:   filepath.Join(source, ".development/runtime/gvisor/bin/runsc"),
		RuntimeRoot: filepath.Join(runtimeRoot, "runsc"),
		SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"),
	})
	registry := core.NewRegistry(nil)
	manager, err := New(Config{Root: root, PackagesRoot: packages,
		UsersRoot: filepath.Join(root, "users"), RuntimeRoot: runtimeRoot,
		ImageRoot:   filepath.Join(source, ".development/runtime/development/rootfs"),
		ImageRecord: filepath.Join(source, ".development/runtime/development/image.json"),
		Driver:      driver, ActivationGateway: NewCommandBusGateway(registry),
	})
	if err != nil {
		t.Fatal(err)
	}
	registerTestActivationCommands(t, registry, manager)
	initializeTestRepository(t, manager, "the8020/dev-core", "Fixture", "fixture@example.test", "Base")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return manager, driver, repository
}

func analysisExec(t *testing.T, driver *RunscDriver, sandbox Sandbox, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := driver.Exec(ctx, sandbox.SandboxID, command)
	if err != nil {
		t.Fatalf("exec %q: %v: %s", command, err, output)
	}
	return string(output)
}

func TestWorkflowAnalysisRuntime(t *testing.T) {
	m, d, repository := analysisRuntime(t)
	ctx := context.Background()
	a, err := m.Create(ctx, "analysisa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, "analysisb")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "cd /workspace/packages/the8020/dev-core; "
	analysisExec(t, d, a, prefix+"printf 'A\\n' > same.txt; sed -i '1s/first/A-first/' disjoint.txt")
	analysisExec(t, d, b, prefix+"printf 'B\\n' > same.txt; sed -i '$s/last/B-last/' disjoint.txt")
	shared, _ := os.ReadFile(filepath.Join(repository, "same.txt"))
	if string(shared) != "base\n" {
		t.Fatal("private write escaped")
	}
	writeTestFile(t, filepath.Join(repository, "untouched.txt"), "host-visible-now\n")
	visible := analysisExec(t, d, b, prefix+"cat untouched.txt; cat same.txt")
	t.Logf("live lower/private: %q", visible)
	if visible != "host-visible-now\nB\n" {
		t.Fatal("live lower/private view mismatch")
	}
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Host update"); err != nil {
		t.Fatal(err)
	}
	// A marker in tmpfs identifies the process generation even though the ID stays.
	analysisExec(t, d, a, "printf running > /tmp/generation-proof")
	started := time.Now()
	ra, err := m.Activate(ctx, a.UserID, ActivationOptions{Description: "A publishes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A activation: duration=%s reset=%v success=%v", time.Since(started), ra.OverlayReset, ra.Success)
	t.Logf("A generation after activation: %s", analysisExec(t, d, a, "if test -e /tmp/generation-proof; then echo same; else echo replaced; fi"))
	started = time.Now()
	rb, err := m.Activate(ctx, b.UserID, ActivationOptions{Description: "B publishes"})
	t.Logf("B activation: duration=%s success=%v status=%s error=%v", time.Since(started), rb.Success, rb.Status, err)
	same, _ := os.ReadFile(filepath.Join(repository, "same.txt"))
	disjoint, _ := os.ReadFile(filepath.Join(repository, "disjoint.txt"))
	t.Logf("published same-line=%q disjoint-line=%q", same, disjoint)
	// Force a real stale capture through the existing merge implementation.
	analysisExec(t, d, b, prefix+"printf 'B-next\\n' > same.txt")
	captured, err := m.captureChanges(ctx, b, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repository, "same.txt"), "A-next\n")
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "A advances after capture"); err != nil {
		t.Fatal(err)
	}
	prepared, err := m.preparePackage(ctx, b, captured[0], "B captured earlier", "B", "b@example.test", nil, t.TempDir())
	t.Logf("stale-capture merge: status=%s conflicts=%q error=%v", prepared.result.Status, prepared.result.Conflicts, err)
	if prepared.result.Status != "conflicted" {
		t.Fatal("expected conflict probe")
	}
	conflicted, _ := os.ReadFile(filepath.Join(prepared.worktrees[len(prepared.worktrees)-1], "same.txt"))
	t.Logf("host merge conflict has markers=%v", strings.Contains(string(conflicted), "<<<<<<<"))
	m.cleanupPreparedActivation(prepared)
	t.Logf("sandbox after conflict cleanup=%q", analysisExec(t, d, b, prefix+"cat same.txt"))
	// Abrupt loss without an explicit checkpoint.
	analysisExec(t, d, b, prefix+"printf crash-new > crash-new.txt; mkdir -p ignored; printf artifact > ignored/artifact")
	if err := d.Kill(ctx, b.SandboxID); err != nil {
		t.Fatal(err)
	}
	_, err = m.EnsureSandbox(ctx, b.UserID)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce loss of the disposable backend too. Ensure trusts the owned
	// bit; explicit Start can recreate once the runtime record is gone.
	if err := d.Delete(ctx, b.SandboxID); err != nil {
		t.Fatal(err)
	}
	b, err = m.Start(ctx, b.UserID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after abrupt loss: %s", analysisExec(t, d, b, prefix+"for f in crash-new.txt ignored/artifact; do if test -f \"$f\"; then echo \"$f=retained\"; else echo \"$f=lost\"; fi; done"))
	// Exercise the canonical helper's actual conflict response. A competing
	// commit between capture and preparation reaches the existing cherry-pick.
	analysisExec(t, d, b, prefix+"printf 'B-helper\\n' >same.txt; printf retained >/tmp/conflict-generation")
	var mutationErr error
	m.driver = analysisPauseDriver{SandboxDriver: d, atPause: func() {
		mutationErr = os.WriteFile(filepath.Join(repository, "same.txt"), []byte("A-helper\n"), 0600)
		if mutationErr == nil {
			_, mutationErr = gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Competing helper publication")
		}
	}}
	conflictOutput := analysisExec(t, d, b, "activate --message 'Helper conflict'; status=$?; printf '\\nhelper_conflict_exit=%s\\n' \"$status\"; test -f /tmp/conflict-generation")
	m.driver = d
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !strings.Contains(conflictOutput, "helper_conflict_exit=3") || !strings.Contains(conflictOutput, `"status":"conflicted"`) || !strings.Contains(conflictOutput, `"conflicts":["same.txt"]`) {
		t.Fatalf("helper conflict status: %s", conflictOutput)
	}
	t.Logf("real helper conflict preserves process generation and returns: %s", conflictOutput)
	// Closing the exact production PTY used by browser and SSH.
	options := backend.ConsoleOptions{Arguments: []string{"/bin/bash", "-lc", "echo $$ >/root/analysis-direct-pid; exec sleep 600"},
		Environment: []string{"HOME=/root", "TERM=xterm-256color", "PATH=" + developmentPath}, WorkingDir: "/root",
		Size: backend.ConsoleSize{Columns: 100, Rows: 30}, Terminal: true}
	terminal, err := d.OpenConsole(ctx, a.SandboxID, options)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, terminal)
	analysisExec(t, d, a, "for i in $(seq 1 50); do test -s /root/analysis-direct-pid && break; sleep .02; done; kill -0 $(cat /root/analysis-direct-pid)")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("direct process after disconnect: %s", analysisExec(t, d, a, "sleep .2; if test -e /proc/$(cat /root/analysis-direct-pid)/stat; then cat /proc/$(cat /root/analysis-direct-pid)/stat; else echo dead; fi"))
	// Native session owner prototype, installed only in this disposable sandbox.
	analysisExec(t, d, a, "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends tmux")
	t.Logf("tmux version=%s", analysisExec(t, d, a, "tmux -V"))
	analysisExec(t, d, a, "tmux -L workflow new-session -d -s first; tmux -L workflow new-session -d -s second; tmux -L workflow set-option -g history-limit 2000")
	before := analysisExec(t, d, a, "tmux -L workflow list-panes -a -F '#{session_name}:#{pane_pid}'")
	authentication := &analysisAuthentication{}
	authentication.loggedIn.Store(true)
	broker, err := platformconsole.New(platformconsole.Config{Authentication: authentication, Development: m})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	httpServer := httptest.NewServer(broker)
	defer httpServer.Close()
	wsConfig, err := websocket.NewConfig("ws"+strings.TrimPrefix(httpServer.URL, "http")+platformconsole.Route, httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	wsConfig.Protocol = []string{"the8020.console.v1"}
	wsConfig.Header.Set("Cookie", "the8020_auth=analysis")
	for i := 0; i < 20; i++ {
		name := []string{"first", "second"}[i%2]
		socket, err := websocket.DialConfig(wsConfig)
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{}, 1)
		go func() {
			for {
				var output []byte
				if websocket.Message.Receive(socket, &output) != nil {
					return
				}
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}()
		if err := websocket.JSON.Send(socket, map[string]any{"type": "open", "target": map[string]any{"kind": "development", "sandboxId": a.SandboxID},
			"arguments":   []string{"/usr/bin/tmux", "-L", "workflow", "attach-session", "-t", name},
			"environment": options.Environment, "workingDirectory": "/root", "columns": 90 + i, "rows": 24}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("attached terminal produced no initial screen")
		}
		if err := websocket.Message.Send(socket, []byte(fmt.Sprintf("printf 'switch-%d\\n'\r", i))); err != nil {
			t.Fatal(err)
		}
		// Verify command execution, not merely survival of the tmux daemon.
		analysisExec(t, d, a, fmt.Sprintf("for i in $(seq 1 60); do tmux -L workflow capture-pane -p -t %s | grep -q '^switch-%d' && exit 0; sleep .05; done; tmux -L workflow capture-pane -p -t %s; tmux -L workflow list-clients; exit 1", name, i, name))
		if err := socket.Close(); err != nil {
			t.Fatal(err)
		}
	}
	authentication.loggedIn.Store(false)
	if socket, err := websocket.DialConfig(wsConfig); err == nil {
		socket.Close()
		t.Fatal("logged-out reattach accepted")
	}
	authentication.loggedIn.Store(true)
	after := analysisExec(t, d, a, "tmux -L workflow list-panes -a -F '#{session_name}:#{pane_pid}'")
	if before != after {
		t.Fatalf("session process changed: before=%q after=%q", before, after)
	}
	t.Logf("tmux survived 20 authenticated WebSocket closes/switches plus logout/authentication gate; processes=%q", after)
	t.Logf("remaining tmux clients=%q", analysisExec(t, d, a, "sleep .1; tmux -L workflow list-clients -F '#{client_name}'"))
	t.Logf("tmux detached screen=%q", analysisExec(t, d, a, "tmux -L workflow capture-pane -p -t first | sed '/^$/d'"))
	// The canonical helper responds before the delayed destruction, then kills its caller.
	analysisExec(t, d, a, prefix+"printf helper > helper.txt")
	started = time.Now()
	output, helperErr := d.Exec(ctx, a.SandboxID, "activate --message 'Helper publication'; sleep 1; printf survived > /root/analysis-helper-survived")
	t.Logf("helper result duration=%s error=%v output=%s", time.Since(started), helperErr, output)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, loadErr := m.loadSandbox(a.UserID)
		if loadErr == nil && state.LastActivationResult != nil && state.LastActivationResult.OverlayReset {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("helper caller survived=%s", analysisExec(t, d, a, "if test -e /root/analysis-helper-survived; then echo yes; else echo no; fi"))
}
