//go:build workflowanalysis

package development

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformconsole "the8020/kernel/console"
	"the8020/kernel/sandbox/backend"
)

// This is the explicit-sync alternative, not a transparent live overlay. The
// wrapper changes only this disposable fixture's mount and initial checkout.
type analysisNativeDriver struct {
	*RunscDriver
	users string
}

func (d analysisNativeDriver) Start(ctx context.Context, start SandboxStart) error {
	workspace := filepath.Join(d.users, start.UserID, "dev-sandbox/workspace")
	repository := filepath.Join(workspace, "the8020/dev-core")
	if _, err := os.Stat(repository); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(repository), 0700); err != nil {
			return err
		}
		if _, err := gitCommand(ctx, filepath.Dir(repository), nil, "clone", "--no-local", filepath.Join(start.Packages, "the8020/dev-core"), repository); err != nil {
			return err
		}
	}
	for i := range start.Mounts {
		if start.Mounts[i].Behavior == MountSandboxSource {
			start.Mounts[i].HostSource = workspace
			start.Mounts[i].Behavior = MountPersistent
		}
	}
	return d.RunscDriver.Start(ctx, start)
}

// Workspace Git runs inside the sandbox. The host imports only a native bundle
// into its own repository; it never runs Git against developer-controlled config,
// hooks, alternates, or .git paths. Production must also bound/admit this stream.
func analysisExportCommit(t *testing.T, d *RunscDriver, sandbox Sandbox, shared, commit string, common ...string) int64 {
	t.Helper()
	ctx := context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	ref := "refs/heads/analysis-export"
	analysisExec(t, d, sandbox, prefix+"git update-ref "+ref+" "+shellQuote(commit))
	path := filepath.Join(t.TempDir(), "private.bundle")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	command := prefix + "git bundle create - " + ref
	for _, base := range common {
		command += " " + shellQuote("^"+base)
	}
	err = d.ExecStream(ctx, sandbox.SandboxID, command, nil, file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("export native bundle: %v, %v", err, closeErr)
	}
	if _, err := gitCommand(ctx, shared, nil, "-c", "fetch.fsckObjects=true", "fetch", path, ref); err != nil {
		t.Fatal(err)
	}
	head, err := gitCommand(ctx, shared, nil, "rev-parse", "FETCH_HEAD")
	if err != nil || strings.TrimSpace(string(head)) != commit {
		t.Fatalf("exported commit changed: %q, %v", head, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func analysisFetchShared(t *testing.T, d *RunscDriver, sandbox Sandbox, shared string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.bundle")
	if _, err := gitCommand(ctx, shared, nil, "bundle", "create", path, "HEAD"); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	output := &boundedBuffer{limit: commandOutputLimit}
	if err := d.ExecStream(ctx, sandbox.SandboxID, "set -e; cd /workspace/packages/the8020/dev-core; cat >/tmp/analysis-shared.bundle; git fetch /tmp/analysis-shared.bundle HEAD:refs/remotes/system/current", file, output); err != nil {
		t.Fatalf("fetch shared bundle inside sandbox: %v: %s", err, output.String())
	}
}

func TestWorkflowAnalysisNative(t *testing.T) {
	m, d, shared := analysisRuntime(t)
	ctx := context.Background()
	m.driver = analysisNativeDriver{d, m.config.UsersRoot}
	for n := 0; n < 10000; n++ {
		writeTestFile(t, filepath.Join(shared, "bench", fmt.Sprintf("%05d.txt", n)), "small file\n")
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "10k files"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	sandbox, err := m.Create(ctx, "nativeproof")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("initial sandbox plus independent 10k checkout=%s", time.Since(started))
	t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
	private := filepath.Join(m.sandboxRootForUser(sandbox.UserID), "workspace/the8020/dev-core")
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	// Build the test workload, rather than copying a host tool or its libraries.
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(private, "ignored/native-probe")
	if err := os.MkdirAll(filepath.Dir(probe), 0700); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, filepath.Join(source, ".development/toolchains/go/bin/go"), "build", "-o", probe, filepath.Join(source, "kernel/development/analysis/native_probe.go"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v: %s", err, output)
	}
	t.Log(strings.TrimSpace(analysisExec(t, d, sandbox, prefix+"./ignored/native-probe")))
	for n := 0; n < 4; n++ {
		started := time.Now()
		analysisExec(t, d, sandbox, prefix+"find bench -type f -exec cat {} + >/dev/null")
		t.Logf("10k traversal sample=%d duration=%s", n, time.Since(started))
	}
	analysisExec(t, d, sandbox, prefix+"git checkout -qb analysis-local; printf 'private\n' >same.txt; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Native private commit'; printf 'untracked\n' >untracked.txt; printf 'ignored\n' >ignored/artifact")
	commit := strings.TrimSpace(analysisExec(t, d, sandbox, prefix+"git rev-parse HEAD"))
	broker, err := platformconsole.New(platformconsole.Config{Authentication: sshProofAuthentication{}, Development: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	terminal, err := broker.CreateTerminal(ctx, "development", sandbox.SandboxID, backend.ConsoleOptions{
		Arguments: []string{"/bin/bash", "-l"}, WorkingDir: "/workspace/packages/the8020/dev-core", Terminal: true,
		Environment: []string{"TERM=xterm-256color", "HOME=/root", "PATH=" + developmentPath}, Size: backend.ConsoleSize{Columns: 90, Rows: 27},
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("stty -echo; export PS1=''; test -t 0; printf '%s' $$ >/tmp/native-tty-before; printf 'TTY-READY\\n'\n")); err != nil {
		t.Fatal(err)
	}
	sequence := retainedUntil(t, attachment, 0, "TTY-READY")
	_ = attachment.Close()
	// Publication reads an immutable Git commit. Later saves and an already-open
	// writable descriptor remain in the primary workspace; no reset/checkout.
	commandDone := make(chan error, 1)
	go func() {
		_, err := d.Exec(ctx, sandbox.SandboxID, prefix+"exec 9<>ignored/artifact; printf '%s' $$ >/tmp/native-pid; printf ready >/tmp/native-ready; for i in $(seq 1 600); do test -f /tmp/native-published && break; sleep .05; done; test -f /tmp/native-published; printf 'late handle write\n' >&9; printf 'after publish\n' >after.txt; printf '%s' $$ >/tmp/native-pid-after")
		commandDone <- err
	}()
	analysisExec(t, d, sandbox, "for i in $(seq 1 600); do test -f /tmp/native-ready && exit 0; sleep .05; done; exit 1")
	analysisExec(t, d, sandbox, prefix+"printf 'later save\n' >same.txt")
	analysisExportCommit(t, d, sandbox, shared, commit)
	if _, err := gitCommand(ctx, shared, nil, "merge", "--ff-only", commit); err != nil {
		t.Fatal(err)
	}
	analysisExec(t, d, sandbox, "touch /tmp/native-published")
	if err := <-commandDone; err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, "cmp /tmp/native-pid /tmp/native-pid-after; echo same-process"); got != "same-process\n" {
		t.Fatal(got)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "private\n" {
		t.Fatalf("published candidate changed with later save: %q, %v", body, err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt ignored/artifact after.txt"); got != "later save\nlate handle write\nafter publish\n" {
		t.Fatalf("later writes lost: %q", got)
	}
	t.Log("immutable commit publication preserved caller PID, cwd, open descriptor, and later saves; schema coordinator excluded")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "new shared\n")
	if got := analysisExec(t, d, sandbox, prefix+"cat untouched.txt"); got != "before\n" {
		t.Fatalf("private checkout changed without explicit sync: %q", got)
	}
	t.Log("TRADEOFF: untouched private files do not follow shared updates until explicit Git synchronization")
	// Native Git exposes real unmerged stages and markers in a separate durable
	// worktree. Resolution and a later shared advance never overwrite the primary
	// editor's files. This remains a publication prototype without schema hooks.
	analysisExec(t, d, sandbox, prefix+"git add -A; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Capture next candidate'")
	commit = strings.TrimSpace(analysisExec(t, d, sandbox, prefix+"git rev-parse HEAD"))
	writeTestFile(t, filepath.Join(shared, "same.txt"), "competing shared save\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Competing shared save"); err != nil {
		t.Fatal(err)
	}
	analysisFetchShared(t, d, sandbox, shared)
	conflict := "/workspace/packages/.conflicts/dev-core"
	analysisExec(t, d, sandbox, prefix+"git worktree add --detach "+conflict+" HEAD")
	conflictPrefix := "set -e; cd " + conflict + "; "
	output, mergeErr := d.Exec(ctx, sandbox.SandboxID, conflictPrefix+"if git -c user.name=Analysis -c user.email=analysis@example.test -c merge.conflictStyle=diff3 merge --no-commit --no-ff refs/remotes/system/current; then exit 0; else exit 3; fi")
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "exit status 3") {
		t.Fatalf("expected conflict exit 3: %q, %v", output, mergeErr)
	}
	if got := analysisExec(t, d, sandbox, conflictPrefix+"git show :1:same.txt; git show :2:same.txt; git show :3:same.txt"); got != "private\nlater save\ncompeting shared save\n" {
		t.Fatalf("incorrect native conflict stages: %q", got)
	}
	if got := analysisExec(t, d, sandbox, conflictPrefix+"cat same.txt"); !strings.Contains(got, "<<<<<<<") || !strings.Contains(got, "|||||||") {
		t.Fatalf("native diff3 markers unavailable: %q", got)
	}
	analysisExec(t, d, sandbox, prefix+"printf 'newer primary save\n' >same.txt")
	analysisExec(t, d, sandbox, conflictPrefix+"printf 'resolved both sides\n' >same.txt; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Resolve both sides'")
	writeTestFile(t, filepath.Join(shared, "arrived-during-resolution.txt"), "another publication\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Advance while resolving"); err != nil {
		t.Fatal(err)
	}
	analysisFetchShared(t, d, sandbox, shared)
	analysisExec(t, d, sandbox, conflictPrefix+"git -c user.name=Analysis -c user.email=analysis@example.test merge --no-edit refs/remotes/system/current")
	resolved := strings.TrimSpace(analysisExec(t, d, sandbox, conflictPrefix+"git rev-parse HEAD"))
	analysisExportCommit(t, d, sandbox, shared, resolved)
	if _, err := gitCommand(ctx, shared, nil, "merge", "--ff-only", resolved); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "resolved both sides\n" {
		t.Fatalf("resolved publication mismatch: %q, %v", body, err)
	}
	if got := analysisExec(t, d, sandbox, conflictPrefix+"cat arrived-during-resolution.txt"); got != "another publication\n" {
		t.Fatalf("retry lost a later shared publication: %q", got)
	}
	t.Log("PASS native conflict exit 3, diff3 markers, all three index stages, git add/commit resolution, and retry after another shared commit")
	t.Log("PASS native bundles cross the existing sandbox stream boundary in both directions; developer repository Git executes only inside the sandbox")
	attachment, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("test -t 0; printf '%s' $$ >/tmp/native-tty-after; printf 'TTY-SURVIVED\\n'\n")); err != nil {
		t.Fatal(err)
	}
	retainedUntil(t, attachment, sequence, "TTY-SURVIVED")
	_ = attachment.Close()
	analysisExec(t, d, sandbox, "cmp /tmp/native-tty-before /tmp/native-tty-after")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS retained native PTY detached across both publications and conflict resolution, reattached to the same Bash PID")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"git rev-parse HEAD; git branch --show-current; cat same.txt untracked.txt ignored/artifact after.txt"); got != commit+"\nanalysis-local\nnewer primary save\nuntracked\nlate handle write\nafter publish\n" {
		t.Fatalf("crash/recreation lost native state: %q", got)
	}
	if got := analysisExec(t, d, sandbox, conflictPrefix+"git rev-parse HEAD; cat same.txt"); got != resolved+"\nresolved both sides\n" {
		t.Fatalf("crash/recreation lost conflict worktree: %q", got)
	}
	t.Log("PASS abrupt runtime kill/delete/recreation preserved private commit, branch, source, untracked and ignored files without checkpoint")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(m.sandboxRootForUser(sandbox.UserID), "workspace")
	transferred := filepath.Join(m.sandboxRootForUser("nativetransfer"), "workspace")
	if err := copyDirectory(ctx, workspace, transferred); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	target, err := m.Create(ctx, "nativetransfer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), target.UserID) })
	if got := analysisExec(t, d, target, prefix+"git rev-parse HEAD; cat same.txt ignored/artifact"); got != commit+"\nnewer primary save\nlate handle write\n" {
		t.Fatalf("transfer lost private state: %q", got)
	}
	if got := analysisExec(t, d, target, conflictPrefix+"git rev-parse HEAD; cat same.txt"); got != resolved+"\nresolved both sides\n" {
		t.Fatalf("transfer lost linked conflict worktree: %q", got)
	}
	t.Log("PASS native archive copy into another user's disposable sandbox after original workspace removal, including linked conflict worktree")
}
