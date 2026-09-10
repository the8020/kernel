package development

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	platformconsole "the8020/kernel/console"
	"the8020/kernel/sandbox/backend"
)

func (d *workspace) request(t *testing.T, action, path, id string) map[string]any {
	t.Helper()
	response, err := d.exchange(context.Background(), action, path, id)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func nativeSparseRuntime(t *testing.T, userID string, assets int) (*Manager, *workspace, Sandbox, string) {
	t.Helper()
	m, _, shared := nativeRuntime(t)
	ctx := context.Background()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < assets; i++ {
		asset := filepath.Join(shared, "assets", fmt.Sprintf("%d.bin", i))
		if err := os.MkdirAll(filepath.Dir(asset), 0700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(asset)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(f, rand.Reader, 1<<20)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("asset fixture: %v, %v", copyErr, closeErr)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "assets"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Tracked random assets"); err != nil {
		t.Fatal(err)
	}
	sandbox, err := m.Create(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
	buildClient := exec.CommandContext(ctx, filepath.Join(source, ".development/toolchains/go/bin/go"), "build", "-o",
		filepath.Join(sandbox.SystemPath, "root/metadata-client"), filepath.Join(source, "kernel/development/testdata/native_client.go"))
	buildClient.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := buildClient.CombinedOutput(); err != nil {
		t.Fatalf("build native metadata regression: %v: %s", err, output)
	}
	sparse, ok := m.workspaceFor(sandbox.SandboxID)
	if !ok {
		t.Fatal("native driver did not open workspace")
	}
	return m, sparse, sandbox, shared
}

func TestNativeSyscalls(t *testing.T) {
	_, workspace, sandbox, shared := nativeSparseRuntime(t, "syscalls", 1)
	host := t.TempDir()
	for _, root := range []string{host, shared} {
		for _, directory := range []string{"ignored", "renamed/directory", "links/directory", "shared-empty", "remove-shared/deep", "recreate-shared"} {
			if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"new", "replace", "invalid", "same"} {
			writeTestFile(t, filepath.Join(root, "rename", name+".txt"), "shared rename\n")
		}
		if err := os.Link(filepath.Join(root, "rename/same.txt"), filepath.Join(root, "rename/alias.txt")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(root, "renamed/target.txt"), "old target\n")
		for name, target := range map[string]string{"from": "source", "target": "target", "remove": "removed", "keep": "kept"} {
			if err := os.Symlink("../../host-only-"+target, filepath.Join(root, "links", name)); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"nested/remove.txt", "nested/deep/remove.txt", "remove-shared/deep/old.txt", "recreate-shared/old.txt"} {
			writeTestFile(t, filepath.Join(root, name), "original\n")
		}
		for _, name := range []string{"nested/keep.txt", "nested/deep/keep.txt"} {
			writeTestFile(t, filepath.Join(root, name), "untouched sibling\n")
		}
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "Native syscall inputs"}} {
		if _, err := gitCommand(context.Background(), shared, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"rename-lower", "symlink-lower", "remove-directories", "remove-shared-directories", ""} {
		command := exec.CommandContext(context.Background(), filepath.Join(sandbox.SystemPath, "root/metadata-client"), strings.Fields(action)...)
		command.Dir = host
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("host Linux %s: %v: %s", action, err, output)
		}
		t.Logf("host Linux: %s", output)
		t.Logf("workspace: %s", nativeExec(t, workspace.RunscDriver, sandbox, "set -e; cd /workspace/packages/the8020/dev-core; /root/metadata-client "+action))
	}
}

func TestNativeWorkspace(t *testing.T) {
	m, sparse, sandbox, shared := nativeSparseRuntime(t, "sparse", 4)
	d, storage := sparse.RunscDriver, sparse.storage
	ctx := context.Background()
	base, err := gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	if entries, err := os.ReadDir(filepath.Join(storage, "git")); err != nil || len(entries) != 0 {
		t.Fatalf("sandbox startup initialized package Git metadata: %v: %v", entries, err)
	}
	if got := nativeExec(t, d, sandbox, prefix+"test ! -e /workspace/borrowed-git; test ! -e /workspace/shared-git; printf '%s\\n' /workspace/git/*; git rev-parse --absolute-git-dir; git remote get-url origin"); got != "/workspace/git/borrowed\n/workspace/git/private\n/workspace/git/shared\n/workspace/git/private/the8020/dev-core/.git\n/workspace/git/shared/the8020/dev-core\n" {
		t.Fatalf("unexpected grouped Git layout: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.config.PackagesRoot, ".meta/activation-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox startup or ordinary Git created publication locks: %v", err)
	}
	failure, failureErr := m.config.ActivationGateway.Activate(ctx, sandbox.UserID, ActivationOptions{})
	if failureErr != nil || failure.Success || failure.Error != "activation description is required" {
		t.Fatalf("activation result lost its preflight error: %+v: %v", failure, failureErr)
	}
	failedSandbox, err := m.Inspect(sandbox.UserID)
	if err != nil || failedSandbox.LastActivationResult == nil || failedSandbox.LastActivationResult.Error != failure.Error {
		t.Fatalf("stored activation result lost its preflight error: %+v: %v", failedSandbox.LastActivationResult, err)
	}
	storageStages := map[string]int64{}
	largePrivateFiles := map[string]int64{}
	measureStorage := func(stage string) {
		t.Helper()
		var bytes int64
		if err := filepath.WalkDir(storage, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == filepath.Join(storage, "lower") {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				if sparse.isSharedGitObject(path, info) {
					return nil
				}
				bytes += info.Size()
				if info.Size() >= 1<<20 {
					relative, _ := filepath.Rel(storage, path)
					largePrivateFiles[relative] = info.Size()
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		storageStages[stage] = bytes
	}
	measureStorage("initialized")
	nativeExec(t, d, sandbox, prefix+"/root/metadata-client gitdir-file")
	directoryBefore := nativeExec(t, d, sandbox, prefix+`stat -c '%d:%i:%a' .; cat "$(git rev-parse --git-path objects/info/alternates)"`)
	nativeExec(t, d, sandbox, prefix+`deno eval 'Deno.writeTextFileSync("same.txt", "private label\n"); console.log(Deno.readTextFileSync("untouched.txt"))'`)
	directoryAfter, directoryErr := d.Exec(ctx, sandbox.SandboxID, prefix+`stat -c '%d:%i:%a' .; cat "$(git rev-parse --git-path objects/info/alternates)"`)
	if directoryErr != nil || string(directoryAfter) != directoryBefore {
		t.Fatalf("source edit changed parent directory identity or lost Git reference: before=%q after=%q error=%v", directoryBefore, directoryAfter, directoryErr)
	}
	measureStorage("deno_label_edit")
	nativeExec(t, d, sandbox, "/root/metadata-client reject-utime /workspace/packages/the8020/dev-core/assets/0.bin")
	measureStorage("unsupported_metadata_request")
	if storageStages["unsupported_metadata_request"] != storageStages["deno_label_edit"] {
		t.Fatal("failed metadata request copied or froze an untouched asset")
	}
	var state struct {
		GoferPID int `json:"goferPid"`
		Sandbox  struct {
			PID int `json:"pid"`
		} `json:"sandbox"`
	}
	if err := readJSON(filepath.Join(d.config.RuntimeRoot, sandbox.SandboxID+"_sandbox:"+sandbox.SandboxID+".state"), &state); err != nil {
		t.Fatal(err)
	}
	processIO := func() map[string]int64 {
		t.Helper()
		values := map[string]int64{}
		for role, pid := range map[string]int{"sentry": state.Sandbox.PID, "gofer": state.GoferPID} {
			body, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				fields := strings.Fields(line)
				count, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				values[role+"_"+strings.TrimSuffix(fields[0], ":")] = count
			}
		}
		return values
	}
	ioBefore := processIO()
	previewText := nativeExec(t, d, sandbox, "activate --json --preview")
	ioAfter := processIO()
	for key, count := range ioBefore {
		ioAfter[key] -= count
	}
	if ioAfter["sentry_wchar"] > 1<<20 || ioAfter["sentry_write_bytes"] > 1<<20 {
		t.Fatalf("small preview still writes asset-sized contents: %v", ioAfter)
	}
	var preview ActivationPreview
	if err := json.Unmarshal([]byte(previewText), &preview); err != nil || len(preview.Packages) != 1 || preview.Packages[0].ChangedFiles != 1 {
		t.Fatalf("real helper preview: %s, %v", previewText, err)
	}
	measureStorage("helper_preview")
	if storageStages["helper_preview"] > 64<<10 {
		t.Fatalf("preview copied asset object contents: %d bytes, files=%v, io=%v", storageStages["helper_preview"], largePrivateFiles, ioAfter)
	}
	nativeExec(t, d, sandbox, prefix+"git checkout -qb private-work; git add same.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Captured private label'")
	privateCommit := strings.TrimSpace(nativeExec(t, d, sandbox, prefix+"git rev-parse HEAD"))
	sparse.request(t, "capture", "the8020/dev-core/same.txt", "candidate")
	nativeExec(t, d, sandbox, prefix+"printf 'later primary save\\n' >same.txt")
	// Prepare from the filesystem capture, even if the editor saves again before
	// Git runs. Only these captured bytes cross into sandbox-owned Git metadata.
	captured, err := os.Open(filepath.Join(storage, "snapshots/candidate/file"))
	if err != nil {
		t.Fatal(err)
	}
	blob := &boundedBuffer{limit: 256}
	exportErr := d.ExecStream(ctx, sandbox.SandboxID, prefix+"git hash-object -w --stdin", captured, blob)
	closeErr := captured.Close()
	if exportErr != nil || closeErr != nil {
		t.Fatalf("import captured bytes: %v, %v", exportErr, closeErr)
	}
	candidate := strings.TrimSpace(nativeExec(t, d, sandbox, prefix+
		"export GIT_INDEX_FILE=/tmp/sparse-capture-index; git read-tree "+base+
		"; git update-index --add --cacheinfo 100644,"+strings.TrimSpace(blob.String())+",same.txt; "+
		"tree=$(git write-tree); printf 'Captured files\\n' | git -c user.name=Fixture -c user.email=fixture@example.test commit-tree \"$tree\" -p "+base))
	measureStorage("snapshot_git_preparation")
	writeTestFile(t, filepath.Join(shared, "same.txt"), "shared label\n")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared advance\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Competing shared label"); err != nil {
		t.Fatal(err)
	}
	sharedCommit, err := gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat untouched.txt"); got != "shared advance\n" {
		t.Fatalf("untouched path stale: %q", got)
	}
	conflict := "/workspace/packages/.conflicts/label"
	resolution := "set -e; cd " + conflict + "; "
	nativeExec(t, d, sandbox, prefix+"git fetch origin; git worktree add --detach --no-checkout "+conflict+" "+candidate+"; cd "+conflict+"; git sparse-checkout set --no-cone /same.txt; git read-tree --reset -u HEAD")
	output, mergeErr := d.Exec(ctx, sandbox.SandboxID, resolution+"git -c user.name=Fixture -c user.email=fixture@example.test -c merge.conflictStyle=diff3 merge --no-edit "+sharedCommit)
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "exit status 1") {
		t.Fatalf("expected native Git conflict: %q, %v", output, mergeErr)
	}
	markers := nativeExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets")
	if !strings.Contains(markers, "<<<<<<<") || !strings.Contains(markers, "|||||||") || !strings.HasSuffix(markers, "base\nprivate label\nshared label\n") {
		t.Fatalf("incorrect native markers/stages: %q", markers)
	}
	measureStorage("native_conflict")
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
	sparse, _ = m.workspaceFor(sandbox.SandboxID)
	if got := nativeExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets"); got != markers {
		t.Fatalf("unresolved native conflict did not survive recreation: %q", got)
	}
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
	if _, err := attachment.Write([]byte("stty -echo; export PS1=''; printf '%s' $$ >/tmp/sparse-tty-before; printf 'TTY-READY\\n'\n")); err != nil {
		t.Fatal(err)
	}
	sequence := retainedUntil(t, attachment, 0, "TTY-READY")
	attachment.Close()
	nativeExec(t, d, sandbox, resolution+"printf 'resolved label\\n' >same.txt; git add same.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve both sides'")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared during resolution\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Advance during resolution"); err != nil {
		t.Fatal(err)
	}
	sharedCommit, err = gitOutput(shared, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	nativeExec(t, d, sandbox, resolution+"git fetch origin; git -c user.name=Fixture -c user.email=fixture@example.test merge --no-edit "+sharedCommit+"; test ! -e assets")
	if got := nativeExec(t, d, sandbox, resolution+"git show HEAD:untouched.txt; git status --porcelain=v1"); got != "shared during resolution\n" {
		t.Fatalf("retry lost shared content or left dirty Git state: %q", got)
	}
	resolved := strings.TrimSpace(nativeExec(t, d, sandbox, resolution+"git rev-parse HEAD"))
	bundleBytes := nativeExportCommit(t, d, sandbox, shared, resolved, sharedCommit)
	if bundleBytes > 64<<10 {
		t.Fatalf("small activation exported asset history: %d bytes", bundleBytes)
	}
	measureStorage("resolved_and_exported")
	if _, err := gitCommand(ctx, shared, nil, "merge", "--ff-only", resolved); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(got) != "resolved label\n" {
		t.Fatalf("resolved source was not published: %q, %v", got, err)
	}
	ack := sparse.request(t, "acknowledge", "the8020/dev-core/same.txt", "candidate")
	if ack["status"] != "retained" || ack["reason"] != "later_content" {
		t.Fatalf("later edit acknowledgement: %v", ack)
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat same.txt untouched.txt"); got != "later primary save\nshared during resolution\n" {
		t.Fatalf("publication lost later/private or live/shared content: %q", got)
	}
	attachment, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("printf '%s' $$ >/tmp/sparse-tty-after; printf 'TTY-SURVIVED\\n'\n")); err != nil {
		t.Fatal(err)
	}
	retainedUntil(t, attachment, sequence, "TTY-SURVIVED")
	attachment.Close()
	nativeExec(t, d, sandbox, "cmp /tmp/sparse-tty-before /tmp/sparse-tty-after")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
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
	sparse, _ = m.workspaceFor(sandbox.SandboxID)
	if got := nativeExec(t, d, sandbox, resolution+"git rev-parse HEAD; cat same.txt; test ! -e assets"); got != resolved+"\nresolved label\n" {
		t.Fatalf("native resolution did not survive recreation: %q", got)
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat same.txt; git rev-parse HEAD"); got != "later primary save\n"+privateCommit+"\n" {
		t.Fatalf("primary work did not survive recreation: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(storage, "base/the8020/dev-core/same.txt")); err != nil || string(got) != "private label\n" {
		t.Fatalf("next original did not survive: %q, %v", got, err)
	}
	reference := nativeExec(t, d, sandbox, prefix+"cat .git")
	nativeExec(t, d, sandbox, prefix+"rm .git")
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
	sparse, _ = m.workspaceFor(sandbox.SandboxID)
	if got := nativeExec(t, d, sandbox, prefix+"test ! -e .git; git --git-dir=/workspace/git/private/the8020/dev-core/.git rev-parse HEAD; cat same.txt"); got != privateCommit+"\nlater primary save\n" {
		t.Fatalf("removed Git reference or private history changed after recreation: %q", got)
	}
	nativeExec(t, d, sandbox, prefix+"printf %s "+shellQuote(reference)+" >.git; git rev-parse HEAD")
	if got := nativeExec(t, d, sandbox, resolution+"git rev-parse HEAD"); got != resolved+"\n" {
		t.Fatalf("removing the Git reference lost the resolved worktree: %q", got)
	}
	measureStorage("recreated")
	assetsCopied := 0
	var logical int64
	inodes := map[[2]uint64]bool{}
	if err := filepath.WalkDir(storage, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == filepath.Join(storage, "lower") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.Contains(path, "/assets/") {
			assetsCopied++
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if sparse.isSharedGitObject(path, info) {
				return nil
			}
			st := info.Sys().(*syscall.Stat_t)
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if !inodes[key] {
				inodes[key] = true
				logical += info.Size()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if assetsCopied != 0 {
		t.Fatalf("copied %d assets", assetsCopied)
	}
	if len(largePrivateFiles) != 0 || logical > 64<<10 {
		t.Fatalf("native workflow copied asset contents into metadata: %v, %d bytes", largePrivateFiles, logical)
	}
	borrowedRoot := filepath.Join(storage, "borrowed/the8020/dev-core/objects")
	var borrowedBytes int64
	if err := filepath.WalkDir(borrowedRoot, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !sparse.isSharedGitObject(name, info) {
			return fmt.Errorf("retained object is not a shared inode: %s", name)
		}
		borrowedBytes += info.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if borrowedBytes < 4<<20 {
		t.Fatal("retention fixture omitted the asset objects")
	}
	objectPath := "/workspace/git/borrowed/the8020/dev-core/objects/" + base[:2] + "/" + base[2:]
	nativeExec(t, d, sandbox, "deno eval "+shellQuote(`
const source = `+strconv.Quote(objectPath)+`;
Deno.readFileSync(source);
let writable = false;
try { Deno.openSync(source, {write: true}).close(); writable = true; } catch {}
if (writable) throw new Error("borrowed object permits writing");
const target = "/workspace/git/private/borrowed-alias";
let linked = false;
try { Deno.linkSync(source, target); linked = true; } catch {}
if (linked) { Deno.removeSync(target); throw new Error("borrowed object escaped into writable storage"); }
`))
	historyWorktree := "/workspace/packages/.conflicts/retained-history"
	history := "set -e; cd " + historyWorktree + "; "
	nativeExec(t, d, sandbox, prefix+"git worktree add --detach --no-checkout "+historyWorktree+" "+privateCommit+"; cd "+historyWorktree+"; git sparse-checkout set --no-cone /same.txt; git read-tree --reset -u HEAD")
	if output, err := d.Exec(ctx, sandbox.SandboxID, history+"git -c user.name=Fixture -c user.email=fixture@example.test -c merge.conflictStyle=diff3 merge --no-edit "+sharedCommit); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("expected retained-history conflict: %s: %v", output, err)
	}
	historyState := nativeExec(t, d, sandbox, history+"cat same.txt; git ls-files --unmerged; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt")
	assetDigest := nativeExec(t, d, sandbox, prefix+"git cat-file blob "+base+":assets/0.bin | sha256sum")
	// Borrowed history must survive the shared repository dropping its refs and
	// collecting unreachable objects. This changes only this disposable fixture.
	refs, err := gitOutput(shared, "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := gitCommand(ctx, shared, nil, "update-ref", "-d", ref); err != nil {
			t.Fatal(err)
		}
	}
	emptyTree, err := gitCommand(ctx, shared, nil, "mktree")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit-tree", strings.TrimSpace(emptyTree), "-m", "Replacement shared history")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"update-ref", "HEAD", strings.TrimSpace(orphan)}, {"read-tree", "--empty"}, {"reflog", "expire", "--expire=now", "--all"}, {"gc", "--prune=now"}} {
		if _, err := gitCommand(ctx, shared, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "cat-file", "-e", base); err == nil {
		t.Fatal("negative fixture retained the old shared commit")
	}
	if output, err := d.Exec(ctx, sandbox.SandboxID, prefix+"git cat-file -e "+base+"; git fsck --full"); err != nil {
		t.Fatalf("shared GC broke native private history: %s: %v", output, err)
	}
	if err := os.RemoveAll(shared); err != nil {
		t.Fatal(err)
	}
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
	sparse, _ = m.workspaceFor(sandbox.SandboxID)
	if got := nativeExec(t, d, sandbox, history+"cat same.txt; git ls-files --unmerged; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt"); got != historyState {
		t.Fatalf("shared removal/recreation changed the unresolved conflict: %q", got)
	}
	if got := nativeExec(t, d, sandbox, prefix+"git cat-file blob "+base+":assets/0.bin | sha256sum"); got != assetDigest {
		t.Fatalf("shared removal/recreation lost the original asset: %q", got)
	}
	nativeExec(t, d, sandbox, history+"printf 'resolved after shared removal\\n' >same.txt; git add same.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve retained history'; git fsck --full")
	// The permanent read-only root also exposes replacement repositories to
	// ordinary Git fetch without rebinding per-package mounts or private refs.
	writeTestFile(t, filepath.Join(shared, "replacement.txt"), "new shared repository\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "replacement.txt"}, {"commit", "-qm", "Replacement package"}, {"gc"}} {
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), args...); err != nil {
			t.Fatal(err)
		}
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat replacement.txt; git rev-parse HEAD"); got != "new shared repository\n"+privateCommit+"\n" {
		t.Fatalf("replacement froze a shared file or replaced private history: %q", got)
	}
	nativeExec(t, d, sandbox, prefix+"git fetch origin; git fsck --full")
	if got := nativeExec(t, d, sandbox, prefix+"git show FETCH_HEAD:replacement.txt"); got != "new shared repository\n" {
		t.Fatalf("running sandbox could not fetch the replacement repository: %q", got)
	}
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
	sparse, _ = m.workspaceFor(sandbox.SandboxID)
	// Retain replacement objects when a file operation needs them, not at startup.
	nativeExec(t, d, sandbox, prefix+"mv replacement.txt renamed-replacement.txt")
	packs, err := filepath.Glob(filepath.Join(shared, ".git/objects/pack/*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("replacement fixture has no packed objects: %v: %v", packs, err)
	}
	packed, err := os.Stat(filepath.Join(borrowedRoot, "pack", filepath.Base(packs[0])))
	if err != nil || !sparse.isSharedGitObject(filepath.Join(borrowedRoot, "pack", filepath.Base(packs[0])), packed) {
		t.Fatalf("replacement pack was not retained by hardlink: %v", err)
	}
	nativeExec(t, d, sandbox, prefix+"git fetch origin; git fsck --full")
	if got := nativeExec(t, d, sandbox, prefix+"git rev-parse HEAD; git show FETCH_HEAD:replacement.txt"); got != privateCommit+"\nnew shared repository\n" {
		t.Fatalf("fetch from replacement changed private history: %q", got)
	}
}
