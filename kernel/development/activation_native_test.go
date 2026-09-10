package development

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	platformconsole "the8020/kernel/console"
	"the8020/kernel/deployment"
	"the8020/kernel/sandbox/backend"
)

type nativeActivationHook struct {
	prepare  func(context.Context, string, []deployment.Candidate) error
	complete func(context.Context, string, bool) error
}

func (h nativeActivationHook) Prepare(ctx context.Context, id string, candidates []deployment.Candidate) error {
	return h.prepare(ctx, id, candidates)
}
func (h nativeActivationHook) Complete(ctx context.Context, id string, activated bool) error {
	return h.complete(ctx, id, activated)
}

func TestNativeActivation(t *testing.T) {
	m, sparse, sandbox, shared := nativeSparseRuntime(t, "activation", 4)
	d, ctx := sparse.RunscDriver, context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	if !t.Run("private_git_initialization_confines_live_paths", func(t *testing.T) {
		guard := &workspace{storage: t.TempDir(), shared: m.config.PackagesRoot}
		for _, name := range []string{"git", "upper"} {
			if err := os.MkdirAll(filepath.Join(guard.storage, name), 0700); err != nil {
				t.Fatal(err)
			}
		}
		outside := t.TempDir()
		alias := filepath.Join(guard.storage, "git/the8020")
		if err := os.Symlink(outside, alias); err != nil {
			t.Fatal(err)
		}
		if err := guard.initializeGit(ctx, "the8020/dev-core"); err == nil {
			t.Fatal("private Git initialization followed a path outside its root")
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Fatalf("Git initialization changed the outside directory: %v, %v", entries, err)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := guard.initializeGit(ctx, "the8020/dev-core"); err != nil {
			t.Fatal(err)
		}
		if reference, err := os.ReadFile(filepath.Join(guard.storage, "upper/the8020/dev-core/.git")); err != nil || string(reference) != "gitdir: /workspace/git/private/the8020/dev-core/.git\n" {
			t.Fatalf("private Git initialization did not install its reference: %q: %v", reference, err)
		}
	}) {
		return
	}
	commitShared := func(message string) {
		t.Helper()
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", message); err != nil {
			t.Fatal(err)
		}
	}
	activate := func(wantExit int) ActivationResult {
		t.Helper()
		text := nativeExec(t, d, sandbox, "activate --json --message 'Native helper integration'; status=$?; test \"$status\" -eq "+strconv.Itoa(wantExit))
		var result ActivationResult
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatalf("decode helper response %q: %v", text, err)
		}
		return result
	}
	loadAttempt := func() activationAttempt {
		t.Helper()
		var attempt activationAttempt
		if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	// Include visible aliases, a host-only alias, and an alias hidden beneath
	// the native Git reference. Only the visible source names may be privatized.
	writeTestFile(t, filepath.Join(shared, "lower-a.txt"), "linked original\n")
	writeTestFile(t, filepath.Join(shared, "link-source.txt"), "linked original\n")
	writeTestFile(t, filepath.Join(shared, "rename-source.txt"), "first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "nested/keep.txt"), "untouched sibling\n")
	for _, name := range []string{"delete.txt", "nested/remove.txt"} {
		writeTestFile(t, filepath.Join(shared, name), "original deletion\n")
	}
	for _, alias := range []string{filepath.Join(shared, "lower-b.txt"), filepath.Join(shared, ".git/hidden-link"), filepath.Join(m.config.Root, "external-link")} {
		if err := os.Link(filepath.Join(shared, "lower-a.txt"), alias); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "lower-a.txt", "lower-b.txt", "link-source.txt", "delete.txt", "nested/remove.txt", "rename-source.txt", "nested/keep.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Lower hardlink fixture")
	nativeExec(t, d, sandbox, "/root/metadata-client lower-links /workspace/packages/the8020/dev-core/lower-a.txt /workspace/packages/the8020/dev-core/lower-b.txt")
	nativeExec(t, d, sandbox, "/root/metadata-client link-lower /workspace/packages/the8020/dev-core/link-source.txt /workspace/packages/the8020/dev-core/link-created.txt")
	for _, filename := range []string{filepath.Join(shared, "lower-a.txt"), filepath.Join(shared, "lower-b.txt"), filepath.Join(shared, "link-source.txt"), filepath.Join(m.config.Root, "external-link")} {
		body, err := os.ReadFile(filename)
		if err != nil || string(body) != "linked original\n" {
			t.Fatalf("copy-up changed a shared/external alias: %s: %q: %v", filename, body, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sparse.storage, "upper/the8020/dev-core/.git/hidden-link")); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("copy-up replaced the Git reference with a hidden lower alias: %v", err)
	}
	nativeExec(t, d, sandbox, prefix+"for i in $(seq 1 8); do printf temporary >.label-$i.tmp; mv .label-$i.tmp same.txt; done")
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("deleted/the8020/dev-core/.label-%d.tmp", i)
		if _, err := os.Stat(filepath.Join(sparse.storage, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary atomic saves accumulated temporary deletion markers: %s: %v", name, err)
		}
	}
	events := make(chan string, 16)
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("expected one candidate")
			}
			candidate := candidates[0]
			if _, err := os.Stat(filepath.Join(candidate.Root, "rename-source.txt")); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("candidate retained the renamed source: %v", err)
			}
			for _, name := range []string{"delete.txt", "nested/remove.txt"} {
				if _, err := os.Stat(filepath.Join(candidate.Root, name)); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("candidate retained deleted path %s: %v", name, err)
				}
				if body, err := os.ReadFile(filepath.Join(shared, name)); err != nil || string(body) != "original deletion\n" {
					return fmt.Errorf("shared deletion preceded validation: %s: %v", name, err)
				}
			}
			for filename, want := range map[string]string{
				filepath.Join(shared, "same.txt"):                "shared label\n",
				filepath.Join(candidate.Root, "same.txt"):        "resolved label\n",
				filepath.Join(candidate.Root, "disjoint.txt"):    "shared newer first\n2\n3\n4\n5\n6\n7\nprivate last\n",
				filepath.Join(candidate.Root, "new.txt"):         "new before capture\n",
				filepath.Join(shared, "rename-source.txt"):       "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n",
				filepath.Join(candidate.Root, "renamed.txt"):     "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n",
				filepath.Join(candidate.Root, "nested/keep.txt"): "shared sibling update\n",
			} {
				body, err := os.ReadFile(filename)
				if err != nil || string(body) != want {
					return fmt.Errorf("schema-before-source %s: %q: %v", filename, body, err)
				}
			}
			for i := 0; i < 4; i++ {
				name := filepath.Join("assets", strconv.Itoa(i)+".bin")
				before, err := os.Stat(filepath.Join(shared, name))
				if err != nil {
					return err
				}
				linked, err := os.Stat(filepath.Join(candidate.Root, name))
				if err != nil || !os.SameFile(before, linked) {
					return fmt.Errorf("validation copied an asset: %s", name)
				}
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'later during schema\\n' >same.txt"); err != nil {
				return fmt.Errorf("agent could not edit during schema preparation: %w", err)
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf x >assets/0.bin"); err != nil {
				return fmt.Errorf("cannot start a new edit while validation holds a hardlink: %w", err)
			}
			for _, root := range []string{shared, candidate.Root} {
				info, err := os.Stat(filepath.Join(root, "assets/0.bin"))
				if err != nil || info.Size() != 1<<20 {
					return fmt.Errorf("new private asset edit changed source/validation: %v", err)
				}
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'recreated during validation\\n' >delete.txt"); err != nil {
				return fmt.Errorf("cannot recreate captured deletion during validation: %w", err)
			}
			var pending activationAttempt
			if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &pending); err != nil {
				return err
			}
			var originalCapture, duplicateID string
			for _, captured := range pending.Packages[0].Captures {
				if captured.Path == "new.txt" {
					originalCapture = captured.ID
				} else {
					duplicateID = captured.ID
				}
			}
			if _, err := sparse.exchange(ctx, "capture", "the8020/dev-core/new.txt", duplicateID); err == nil {
				return errors.New("duplicate snapshot capture unexpectedly succeeded")
			}
			registered, err := os.ReadFile(filepath.Join(sparse.storage, "snapshots/.pending/the8020/dev-core/new.txt"))
			if err != nil || string(registered) != originalCapture {
				return fmt.Errorf("failed recapture lost prior capture registration: %q: %v", registered, err)
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"rm new.txt"); err != nil {
				return fmt.Errorf("cannot delete a captured new file: %w", err)
			}
			events <- "prepare: complete view, old shared source, later private edit, new asset edit"
			return nil
		},
		complete: func(_ context.Context, _ string, activated bool) error {
			if !activated {
				return errors.New("unexpected schema rollback")
			}
			body, err := os.ReadFile(filepath.Join(shared, "same.txt"))
			if err != nil || string(body) != "resolved label\n" {
				return errors.New("schema completion preceded source publication")
			}
			events <- "complete: new source visible"
			return nil
		},
	})
	nativeExec(t, d, sandbox, prefix+"printf 'private label\\n' >same.txt; mkdir -p ignored; printf 'keep ignored\\n' >ignored/keep.txt; rm delete.txt nested/remove.txt; printf 'new before capture\\n' >new.txt")
	nativeExec(t, d, sandbox, prefix+"mv rename-source.txt renamed.txt")
	writeTestFile(t, filepath.Join(shared, "same.txt"), "shared label\n")
	writeTestFile(t, filepath.Join(shared, "disjoint.txt"), "shared first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "rename-source.txt"), "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "nested/keep.txt"), "shared sibling update\n")
	commitShared("First upstream update")
	if got := nativeExec(t, d, sandbox, prefix+"test -d nested; cat nested/keep.txt"); got != "shared sibling update\n" {
		t.Fatalf("nested deletion hid its live sibling: %q", got)
	}
	nativeExec(t, d, sandbox, prefix+"sed -i 's/^last$/private last/' disjoint.txt")
	writeTestFile(t, filepath.Join(shared, "disjoint.txt"), "shared newer first\n2\n3\n4\n5\n6\n7\nlast\n")
	commitShared("Second upstream update")
	previewHead, err := m.sharedHead("the8020/dev-core")
	if err != nil {
		t.Fatal(err)
	}
	filePreview, err := m.capturePackage(ctx, sparse, sandbox, "the8020/dev-core", "", previewHead, false)
	if err != nil {
		t.Fatal(err)
	}
	wantedDiffs := map[string][]string{
		"same.txt":          {"-base\n", "+private label\n"},
		"new.txt":           {"+new before capture\n"},
		"delete.txt":        {"-original deletion\n"},
		"rename-source.txt": {"-first\n"},
		"renamed.txt":       {"+first\n"},
	}
	for _, file := range filePreview.Captures {
		want, ok := wantedDiffs[file.Path]
		if !ok {
			continue
		}
		diff, err := previewFileDiff(ctx, sparse, "the8020/dev-core", file)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range want {
			if !strings.Contains(diff.Text, text) {
				t.Fatalf("preview %s = %+v, missing %q", file.Path, diff, text)
			}
		}
		if strings.Contains(diff.Text, "shared label") || strings.Contains(diff.Text, "shared rename first") || strings.Contains(diff.Text, sparse.storage) {
			t.Fatalf("diff must show the path's original and private edit only: %+v", diff)
		}
		delete(wantedDiffs, file.Path)
	}
	if len(wantedDiffs) != 0 {
		t.Fatalf("missing preview files: %v", wantedDiffs)
	}
	first := activate(3)
	if first.Status != "conflicted" || len(first.Packages) != 1 || strings.Join(first.Packages[0].Conflicts, ",") != "same.txt" {
		t.Fatalf("expected native helper conflict: %+v", first)
	}
	attempt := loadAttempt()
	for _, capture := range attempt.Packages[0].Captures {
		if _, err := sparse.exchange(ctx, "release", "the8020/dev-core/"+capture.Path, capture.ID); err == nil {
			t.Fatal("released an unacknowledged conflict capture")
		}
		if _, err := os.Stat(filepath.Join(sparse.storage, "snapshots", capture.ID)); err != nil {
			t.Fatal("rejected release removed a pending capture", err)
		}
	}
	worktree := attempt.Packages[0].Worktree
	resolution := "set -e; cd " + shellQuote(worktree) + "; "
	markers := nativeExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets")
	if !strings.Contains(markers, "|||||||") || !strings.HasSuffix(markers, "base\nprivate label\nshared label\n") {
		t.Fatalf("incorrect helper conflict originals: %q", markers)
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
	if got := nativeExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets"); got != markers {
		t.Fatal("helper conflict did not survive recreation")
	}
	nativeExec(t, d, sandbox, prefix+"test lower-a.txt -ef lower-b.txt; test \"$(cat lower-b.txt)\" = 'private linked'")
	nativeExec(t, d, sandbox, prefix+"test link-source.txt -ef link-created.txt; test \"$(cat link-created.txt)\" = 'private linked'")
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
	if _, err := attachment.Write([]byte("stty -echo; export PS1=''; printf '%s' $$ >/tmp/activation-tty-before; printf 'TTY-READY\\n'\n")); err != nil {
		t.Fatal(err)
	}
	sequence := retainedUntil(t, attachment, 0, "TTY-READY")
	attachment.Close()
	conflictUI := func(request map[string]any, wantFailure bool) map[string]any {
		t.Helper()
		request["worktree"] = worktree
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := m.Shell(ctx, sandbox.UserID, "printf %s "+shellQuote(string(body))+" | deno run --allow-read --allow-write --allow-run=/usr/bin/git --allow-env=DEVELOPMENT_USER_ID /workspace/scripts/activation-conflicts.ts")
		if wantFailure {
			if err == nil {
				t.Fatal("conflict editor accepted a stale write")
			}
			return nil
		}
		if err != nil {
			t.Fatalf("native conflict editor: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(response.Output), &result); err != nil {
			t.Fatalf("conflict editor response %q: %v", response.Output, err)
		}
		return result
	}
	opened := conflictUI(map[string]any{"action": "read", "path": "same.txt"}, false)
	if opened["original"] != "base\n" || opened["shared"] != "shared label\n" {
		t.Fatalf("editor lost Git stages: %+v", opened)
	}
	nativeExec(t, d, sandbox, resolution+"printf 'agent edit after UI opened\\n' >same.txt")
	conflictUI(map[string]any{"action": "save", "path": "same.txt", "version": opened["version"], "content": "stale browser edit\n"}, true)
	opened = conflictUI(map[string]any{"action": "read", "path": "same.txt"}, false)
	conflictUI(map[string]any{"action": "save", "path": "same.txt", "version": opened["version"], "content": "resolved label\n"}, false)
	conflictUI(map[string]any{"action": "finish"}, false)
	nativeExec(t, d, sandbox, prefix+"printf 'later primary save\\n' >same.txt")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared during resolution\n")
	commitShared("Shared advancement during resolution")
	second := activate(0)
	if !second.Success || second.Status != "committed" {
		t.Fatalf("helper failed native retry/publication: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(shared, "rename-source.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication retained the renamed source: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "renamed.txt")); err != nil || string(body) != "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n" {
		t.Fatalf("publication lost the shared edit of a renamed file: %q: %v", body, err)
	}
	nativeExec(t, d, sandbox, prefix+"test ! -e rename-source.txt; test \"$(head -n 1 renamed.txt)\" = 'shared rename first'")
	if body, err := os.ReadFile(filepath.Join(shared, "new.txt")); err != nil || string(body) != "new before capture\n" {
		t.Fatalf("activation did not publish its captured new file: %q: %v", body, err)
	}
	nativeExec(t, d, sandbox, prefix+"test ! -e new.txt")
	for _, name := range []string{"delete.txt", "nested/remove.txt"} {
		if _, err := os.Stat(filepath.Join(shared, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("activation failed to publish deletion %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(sparse.storage, "base/the8020/dev-core", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deletion retained an obsolete original for %s: %v", name, err)
		}
	}
	if got := nativeExec(t, d, sandbox, prefix+"test ! -e nested/remove.txt; cat delete.txt"); got != "recreated during validation\n" {
		t.Fatalf("publication lost a later recreation: %q", got)
	}
	writeTestFile(t, filepath.Join(shared, "nested/remove.txt"), "shared recreation\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "nested/remove.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Recreate a published deletion")
	if got := nativeExec(t, d, sandbox, prefix+"cat nested/remove.txt"); got != "shared recreation\n" {
		t.Fatalf("published deletion still hides later shared creation: %q", got)
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat same.txt untouched.txt ignored/keep.txt disjoint.txt"); got != "later during schema\nshared during resolution\nkeep ignored\nshared newer first\n2\n3\n4\n5\n6\n7\nprivate last\n" {
		t.Fatalf("activation lost later, ignored, or merged work: %q", got)
	}
	attachment, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("printf '%s' $$ >/tmp/activation-tty-after; printf 'TTY-SURVIVED\\n'\n")); err != nil {
		t.Fatal(err)
	}
	retainedUntil(t, attachment, sequence, "TTY-SURVIVED")
	attachment.Close()
	nativeExec(t, d, sandbox, "cmp /tmp/activation-tty-before /tmp/activation-tty-after")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, capture := range attempt.Packages[0].Captures {
		for _, payload := range []string{"file", "base"} {
			if _, err := os.Stat(filepath.Join(sparse.storage, "snapshots", capture.ID, payload)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed activation retained its capture payload", err)
			}
		}
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
	for _, capture := range attempt.Packages[0].Captures {
		name := "the8020/dev-core/" + capture.Path
		answer, err := sparse.exchange(ctx, "acknowledge", name, capture.ID)
		if err != nil || answer["status"] != "already_acknowledged" {
			t.Fatalf("recreated receipt lost acknowledgement: %v: %v", answer, err)
		}
		if _, err := sparse.exchange(ctx, "release", name, capture.ID); err != nil {
			t.Fatal("recreated release was not idempotent", err)
		}
		if _, err := sparse.exchange(ctx, "capture", "the8020/dev-core/same.txt", capture.ID); err == nil || !strings.Contains(err.Error(), "file exists") {
			t.Fatal("released capture ID was not reserved", err)
		}
	}
	for _, want := range []string{"prepare: complete view, old shared source, later private edit, new asset edit", "complete: new source visible"} {
		select {
		case event := <-events:
			if event != want {
				t.Fatalf("schema event = %q, want %q", event, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing schema boundary event")
		}
	}
	third := activate(3)
	if third.Status != "conflicted" {
		t.Fatalf("expected later-edit merge conflict: %+v", third)
	}
	attempt = loadAttempt()
	next := attempt.Packages[0].Worktree
	if got := nativeExec(t, d, sandbox, "git -C "+shellQuote(next)+" ls-files -- new.txt"); got != "" {
		t.Fatalf("next activation lost deletion of a captured new file: %q", got)
	}
	if got := nativeExec(t, d, sandbox, "git -C "+shellQuote(next)+" show HEAD:delete.txt; git -C "+shellQuote(next)+" ls-files -u -- delete.txt"); got != "recreated during validation\n" {
		t.Fatalf("recreated deletion did not merge as a new file: %q", got)
	}
	if got := nativeExec(t, d, sandbox, "git -C "+shellQuote(next)+" show :1:same.txt"); got != "private label\n" {
		t.Fatalf("next merge used an original the editor never saw: %q", got)
	}
	nativeExec(t, d, sandbox, "set -e; cd "+shellQuote(next)+"; printf 'resolved later label\\n' >same.txt; git add same.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve later label'")
	// Both failures retain this same native resolution and the original capture.
	// Validation must neither consume later edits nor publish stale candidates.
	preparations, rollbacks, completions := 0, 0, 0
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			preparations++
			if len(candidates) != 1 {
				return errors.New("expected one retry candidate")
			}
			if body, err := os.ReadFile(filepath.Join(candidates[0].Root, "same.txt")); err != nil || string(body) != "resolved later label\n" {
				return fmt.Errorf("retry lost native resolution: %q: %v", body, err)
			}
			switch preparations {
			case 1:
				if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'private after failed validation\\n' >same.txt"); err != nil {
					return err
				}
				return errors.New("fixture schema validation rejected the candidate")
			case 2:
				if err := os.WriteFile(filepath.Join(shared, "untouched.txt"), []byte("shared during validation\n"), 0644); err != nil {
					return err
				}
				_, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Shared changed during validation")
				return err
			case 3:
				if body, err := os.ReadFile(filepath.Join(candidates[0].Root, "untouched.txt")); err != nil || string(body) != "shared during validation\n" {
					return fmt.Errorf("retry omitted latest shared change: %q: %v", body, err)
				}
				return nil
			default:
				return errors.New("unexpected validation retry")
			}
		},
		complete: func(_ context.Context, _ string, activated bool) error {
			if activated {
				completions++
			} else {
				rollbacks++
			}
			return nil
		},
	})
	for failure := 1; failure <= 2; failure++ {
		failed := activate(3)
		if failed.Success || failed.Status != "failed" {
			t.Fatalf("expected validation/source failure %d: %+v", failure, failed)
		}
		pending := loadAttempt()
		if pending.ID != attempt.ID || pending.Phase != "preparing" {
			t.Fatalf("failure replaced or consumed the native attempt: %+v", pending)
		}
		if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "resolved label\n" {
			t.Fatalf("failure published candidate source: %q: %v", body, err)
		}
		if got := nativeExec(t, d, sandbox, prefix+"cat same.txt"); got != "private after failed validation\n" {
			t.Fatalf("failure consumed a later edit: %q", got)
		}
	}
	if preparations != 2 || rollbacks != 2 || completions != 0 {
		t.Fatalf("incorrect failure handshake: prepare=%d rollback=%d complete=%d", preparations, rollbacks, completions)
	}
	sparse.controlMu.Lock()
	sparse.control = &dropReleaseReply{Conn: sparse.control}
	sparse.controlMu.Unlock()
	cleanupFailed := activate(3)
	if cleanupFailed.Success || loadAttempt().Phase != "published" || preparations != 3 || rollbacks != 3 || completions != 1 {
		t.Fatalf("release reply failure lost its published attempt: %+v", cleanupFailed)
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
	retried := activate(0)
	if !retried.Success || preparations != 3 || rollbacks != 3 || completions != 2 {
		t.Fatalf("failed to continue the retained activation: %+v; prepare=%d rollback=%d complete=%d", retried, preparations, rollbacks, completions)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "resolved later label\n" {
		t.Fatalf("retry lost resolved content: %q: %v", body, err)
	}
	if got := nativeExec(t, d, sandbox, prefix+"cat same.txt untouched.txt"); got != "private after failed validation\nshared during validation\n" {
		t.Fatalf("retry lost later/private or shared work: %q", got)
	}
	writeTestFile(t, filepath.Join(shared, "directory-intent/file.txt"), "directory original\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent/file.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Directory activation fixture")
	nativeExec(t, d, sandbox, prefix+"rm -r directory-intent")
	changes := sparse.request(t, "changes", "the8020/dev-core", "directory-intent")
	if got := fmt.Sprint(changes["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("directory removal was missing from activation changes: %v", changes)
	}
	writeTestFile(t, filepath.Join(shared, "directory-intent/file.txt"), "shared directory edit\n")
	writeTestFile(t, filepath.Join(shared, "directory-intent/new.txt"), "new shared child\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared edit and addition under removed directory")
	currentLabel, err := os.ReadFile(filepath.Join(shared, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	nativeExec(t, d, sandbox, prefix+"printf %s "+shellQuote(string(currentLabel))+" >same.txt")
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(_ context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("directory fixture expected one package")
			}
			if _, err := os.Stat(filepath.Join(candidates[0].Root, "directory-intent/file.txt")); !os.IsNotExist(err) {
				return fmt.Errorf("directory candidate retained the resolved deletion: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(candidates[0].Root, "directory-intent/new.txt")); err != nil || string(got) != "new shared child\n" {
				return fmt.Errorf("directory candidate lost the shared addition: %q: %v", got, err)
			}
			return nil
		},
		complete: func(context.Context, string, bool) error { return nil },
	})
	directoryConflict := activate(3)
	if directoryConflict.Status != "conflicted" || len(directoryConflict.Packages) != 1 || fmt.Sprint(directoryConflict.Packages[0].Conflicts) != "[directory-intent/file.txt]" {
		t.Fatalf("directory deletion did not produce the native modify/delete conflict: %+v", directoryConflict)
	}
	directoryAttempt := loadAttempt()
	directoryWorktree := directoryAttempt.Packages[0].Worktree
	if got := nativeExec(t, d, sandbox, "git -C "+shellQuote(directoryWorktree)+" show :1:directory-intent/file.txt; git -C "+shellQuote(directoryWorktree)+" show :3:directory-intent/file.txt"); got != "directory original\nshared directory edit\n" {
		t.Fatalf("directory conflict lost native stages: %q", got)
	}
	nativeExec(t, d, sandbox, "git -C "+shellQuote(directoryWorktree)+" rm directory-intent/file.txt; git -C "+shellQuote(directoryWorktree)+" -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve directory deletion'")
	if result := activate(0); !result.Success {
		t.Fatalf("directory publication failed: %+v", result)
	}
	if got := nativeExec(t, d, sandbox, prefix+"test ! -e directory-intent/file.txt; cat directory-intent/new.txt"); got != "new shared child\n" {
		t.Fatalf("directory publication left the workspace stale: %q", got)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[]" {
		t.Fatalf("completed directory intent remains pending: %s", got)
	}
	nativeExec(t, d, sandbox, prefix+"rm -r directory-intent")
	directoryPreparations := 0
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error {
			directoryPreparations++
			if directoryPreparations == 1 {
				// This later create/remove cycle must remain pending even though
				// the directory is absent both before and after it.
				_, err := d.Exec(ctx, sandbox.SandboxID, prefix+"mkdir directory-intent; printf later >directory-intent/temporary; rm -r directory-intent")
				return err
			}
			var attempt activationAttempt
			if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
				return err
			}
			if len(attempt.Packages) != 1 || len(attempt.Packages[0].Captures) != 0 {
				return errors.New("directory-only fixture unexpectedly captured files")
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, "test ! -e "+shellQuote(attempt.Packages[0].Worktree+"/assets")); err != nil {
				return fmt.Errorf("directory-only worktree materialized assets: %w", err)
			}
			return nil
		},
		complete: func(context.Context, string, bool) error { return nil },
	})
	sparse.control = &dropReleaseReply{Conn: sparse.control}
	failedDirectory := activate(3)
	if failedDirectory.Success || !strings.Contains(failedDirectory.Error, "lost capture-release reply") {
		t.Fatalf("directory fixture did not interrupt after acknowledgement: %+v", failedDirectory)
	}
	pendingDirectory := loadAttempt()
	if pendingDirectory.Phase != "published" || len(pendingDirectory.Packages[0].Directories) != 1 {
		t.Fatalf("directory attempt was not retained: %+v", pendingDirectory)
	}
	oldDirectoryCapture := pendingDirectory.Packages[0].Directories[0]
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("publication consumed a later directory operation: %s", got)
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
	if result := activate(0); !result.Success || directoryPreparations != 1 {
		t.Fatalf("recreated directory retry repeated preparation: %+v, prepares=%d", result, directoryPreparations)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("recreated acknowledgement consumed later directory intent: %s", got)
	}
	// Git versions files. A remaining directory-only intent needs no file
	// checkout; acknowledging this later generation restores live lower paths.
	if result := activate(0); !result.Success || directoryPreparations != 2 {
		t.Fatalf("directory-only activation failed: %+v, prepares=%d", result, directoryPreparations)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[]" {
		t.Fatalf("later directory generation was not acknowledged: %s", got)
	}
	if _, err := sparse.exchange(ctx, "acknowledge-directory", "the8020/dev-core/"+oldDirectoryCapture.Path, oldDirectoryCapture.ID); err == nil || !strings.Contains(err.Error(), "stale file handle") {
		t.Fatalf("stale directory acknowledgement was not rejected: %v", err)
	}
	writeTestFile(t, filepath.Join(shared, "directory-intent/after.txt"), "shared after directory publication\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent/after.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared recreation after directory publication")
	if got := nativeExec(t, d, sandbox, prefix+"cat directory-intent/after.txt"); got != "shared after directory publication\n" {
		t.Fatalf("acknowledged directory stayed obsolete: %q", got)
	}
	writeTestFile(t, filepath.Join(shared, "ignored/shared/tracked.txt"), "newly tracked upstream\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "-f", "ignored/shared/tracked.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Track a new file under an ignored directory")
	nativeExec(t, d, sandbox, prefix+"rm -r ignored/shared")
	m.SetSchemaDeployment(nativeActivationHook{
		prepare:  func(context.Context, string, []deployment.Candidate) error { return nil },
		complete: func(context.Context, string, bool) error { return nil },
	})
	if result := activate(0); !result.Success || result.Status != "committed" {
		t.Fatalf("newly shared tracked file was treated as ignored: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(shared, "ignored/shared/tracked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked deletion under an ignored directory was not published: %v", err)
	}
	nativeExec(t, d, sandbox, "deno eval "+shellQuote(`
const root = "/workspace/packages/the8020/workflow-new";
Deno.mkdirSync(root + "/programs/hello", {recursive: true});
Deno.writeTextFileSync(root + "/package.toml", "schema = 1\n");
Deno.writeTextFileSync(root + "/programs/hello/program.toml", "schema = 1\nentrypoint = \"main.ts\"\n");
Deno.writeTextFileSync(root + "/programs/hello/main.ts", "export default () => 'created';\n");
Deno.writeTextFileSync("/workspace/packages/the8020/dev-core/same.txt", "edited alongside new package\n");
`))
	created := activate(0)
	if !created.Success || len(created.Packages) != 2 {
		t.Fatalf("ordinary package creation was omitted from the activation batch: %+v", created)
	}
	if body, err := os.ReadFile(filepath.Join(m.config.PackagesRoot, "the8020/workflow-new/programs/hello/main.ts")); err != nil || string(body) != "export default () => 'created';\n" {
		t.Fatalf("new package was not published: %q: %v", body, err)
	}
	nativeExec(t, d, sandbox, "rm -r /workspace/packages/the8020/workflow-new")
	newRoot := filepath.Join(m.config.PackagesRoot, "the8020/workflow-new")
	writeTestFile(t, filepath.Join(newRoot, "programs/hello/main.ts"), "export default () => 'upstream edit';\n")
	writeTestFile(t, filepath.Join(newRoot, "upstream-new.txt"), "added while package was removed\n")
	if _, err := gitCommand(ctx, newRoot, nil, "add", "upstream-new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, newRoot, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Concurrent shared edit"); err != nil {
		t.Fatal(err)
	}
	deleted := activate(3)
	if deleted.Status != "conflicted" || len(deleted.Packages) != 1 || len(deleted.Packages[0].Conflicts) != 2 {
		t.Fatalf("package deletion lost concurrent edits: %+v", deleted)
	}
	deletionAttempt := loadAttempt()
	deletionWorktree := deletionAttempt.Packages[0].Worktree
	nativeExec(t, d, sandbox, "git -C "+shellQuote(deletionWorktree)+" rm programs/hello/main.ts upstream-new.txt && git -C "+shellQuote(deletionWorktree)+" -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve package removal'")
	if result := activate(0); !result.Success {
		t.Fatalf("resolved package deletion: %+v", result)
	}
	if _, err := os.Stat(newRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed package still exists: %v", err)
	}
	upstreamID := "the8020/workflow-upstream-removed"
	nativeExec(t, d, sandbox, "mkdir -p /workspace/packages/"+upstreamID+"; printf 'schema = 1\\n' >/workspace/packages/"+upstreamID+"/package.toml; printf 'original\\n' >/workspace/packages/"+upstreamID+"/label.txt")
	activate(0)
	nativeExec(t, d, sandbox, "printf 'private after upstream deletion\\n' >/workspace/packages/"+upstreamID+"/label.txt")
	if err := os.RemoveAll(filepath.Join(m.config.PackagesRoot, upstreamID)); err != nil {
		t.Fatal(err)
	}
	removedUpstream := activate(3)
	if len(removedUpstream.Packages) != 1 || len(removedUpstream.Packages[0].Conflicts) != 1 || removedUpstream.Packages[0].Conflicts[0] != "label.txt" {
		t.Fatalf("upstream removal hid private work: %+v", removedUpstream)
	}
	resolutionRoot := removedUpstream.Packages[0].ConflictWorktree
	nativeExec(t, d, sandbox, "git -C "+shellQuote(resolutionRoot)+" rm label.txt && git -C "+shellQuote(resolutionRoot)+" -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Accept upstream removal'")
	if result := activate(0); !result.Success {
		t.Fatalf("accept upstream removal: %+v", result)
	}
	if err := os.MkdirAll(filepath.Join(shared, "symbolic"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"remove", "from", "changed"} {
		if err := os.Symlink("/tmp/original-link-target", filepath.Join(shared, "symbolic", name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "symbolic"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared symlink fixtures")
	nativeExec(t, d, sandbox, prefix+"rm symbolic/remove && mv symbolic/from symbolic/moved && ln -sfn /tmp/private-link-target symbolic/changed")
	if err := os.Remove(filepath.Join(shared, "symbolic/changed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/shared-link-target", filepath.Join(shared, "symbolic/changed")); err != nil {
		t.Fatal(err)
	}
	commitShared("Concurrent shared symlink edit")
	symlinkConflict := activate(3)
	if len(symlinkConflict.Packages) != 1 || len(symlinkConflict.Packages[0].Conflicts) != 1 || symlinkConflict.Packages[0].Conflicts[0] != "symbolic/changed" {
		t.Fatalf("symlink changes did not retain a native conflict: %+v", symlinkConflict)
	}
	worktree = symlinkConflict.Packages[0].ConflictWorktree
	linkConflict := conflictUI(map[string]any{"action": "read", "path": "symbolic/changed"}, false)
	if linkConflict["binary"] != true || linkConflict["hasShared"] != true || linkConflict["original"] != "/tmp/original-link-target" || linkConflict["private"] != "/tmp/private-link-target" || linkConflict["shared"] != "/tmp/shared-link-target" {
		t.Fatalf("conflict helper did not expose the link targets and side choices: %+v", linkConflict)
	}
	conflictUI(map[string]any{"action": "shared", "path": "symbolic/changed", "version": linkConflict["version"]}, false)
	conflictUI(map[string]any{"action": "finish"}, false)
	if result := activate(0); !result.Success {
		t.Fatalf("symlink resolution publication: %+v", result)
	}
	for _, name := range []string{"remove", "from"} {
		if _, err := os.Lstat(filepath.Join(shared, "symbolic", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink deletion was not published: %s: %v", name, err)
		}
	}
	for name, target := range map[string]string{"moved": "/tmp/original-link-target", "changed": "/tmp/shared-link-target"} {
		if value, err := os.Readlink(filepath.Join(shared, "symbolic", name)); err != nil || value != target {
			t.Fatalf("published symlink %s = %q: %v", name, value, err)
		}
	}
}

func TestNativeRename(t *testing.T) {
	if !t.Run("roots", nativePackageRenames) {
		return
	}

	m, sparse, sandbox, shared := nativeSparseRuntime(t, "rename", 4)
	m.SetSchemaDeployment(nativeActivationHook{
		prepare:  func(context.Context, string, []deployment.Candidate) error { return nil },
		complete: func(context.Context, string, bool) error { return nil },
	})
	d, ctx := sparse.RunscDriver, context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	commitShared := func(message string) {
		t.Helper()
		if _, err := gitCommand(ctx, shared, nil, "add", "-A"); err != nil {
			t.Fatal(err)
		}
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", message); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"assets/nested/merge.txt": "first\n2\n3\n4\n5\n6\n7\nlast\n",
		"assets/conflict.txt":     "original title\n",
		"assets/annotated.txt":    "original heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n",
		"assets/removed.txt":      "remove me\n",
		"assets/untouched.txt":    "original unchanged text\n",
		"assets/after-rename.txt": "original before rename\n",
	} {
		writeTestFile(t, filepath.Join(shared, name), body)
	}
	commitShared("Directory rename inputs")
	nativeExec(t, d, sandbox, prefix+"printf 'private title\n' >assets/conflict.txt; sed -i 's/original heading/private heading/' assets/annotated.txt; sed -i 's/^last$/private last/' assets/nested/merge.txt; printf 'new file\n' >assets/new.txt; rm assets/removed.txt; mkdir -p empty/nested target; printf occupied >target/keep")
	nativeExec(t, d, sandbox, prefix+"exec 9<assets/untouched.txt; cd assets/nested; mv /workspace/packages/the8020/dev-core/assets /workspace/packages/the8020/dev-core/images; test \"$(pwd -P)\" = /workspace/packages/the8020/dev-core/images/nested; test \"$(cat <&9)\" = 'original unchanged text'; cd ../..; mv images artwork; mv artwork/0.bin artwork/icon.bin; mv empty renamed-empty; test -d renamed-empty/nested; test ! -e assets; test ! -e images; test ! -e empty; test ! -e artwork/removed.txt; test \"$(cat artwork/new.txt)\" = 'new file'; test \"$(cat artwork/conflict.txt)\" = 'private title'; if mv -T artwork target 2>/dev/null; then exit 1; fi; test -f artwork/icon.bin; test -f target/keep")
	nativeExec(t, d, sandbox, prefix+"printf 'edited after rename\n' >artwork/after-rename.txt")
	asset, err := os.ReadFile(filepath.Join(shared, "assets/0.bin"))
	if err != nil {
		t.Fatal(err)
	}
	wantAsset := fmt.Sprintf("%x", sha256.Sum256(asset))
	if got := strings.Fields(nativeExec(t, d, sandbox, prefix+"sha256sum artwork/icon.bin")); len(got) == 0 || got[0] != wantAsset {
		t.Fatalf("renamed asset contents: %v", got)
	}
	noAssetCopies := func() {
		t.Helper()
		if err := filepath.WalkDir(sparse.storage, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(sparse.storage, name)
			if err != nil {
				return err
			}
			if relative == "lower" || relative == "borrowed" {
				return filepath.SkipDir
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Size() >= 1<<20 {
					return fmt.Errorf("rename copied asset-sized content into %s (%d bytes)", relative, info.Size())
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	noAssetCopies()
	// A failed metadata checkpoint must restore both the moved private subtree
	// and an existing destination, without removing the rollback backup first.
	failurePath := filepath.Join(sparse.storage, "snapshots/.references-new")
	if err := os.Mkdir(failurePath, 0700); err != nil {
		t.Fatal(err)
	}
	nativeExec(t, d, sandbox, prefix+"mkdir empty-target; if mv -T artwork empty-target 2>/dev/null; then exit 1; fi; test -f artwork/icon.bin; test \"$(cat artwork/new.txt)\" = 'new file'; test -d empty-target; test ! -e empty-target/icon.bin")
	if err := os.Remove(failurePath); err != nil {
		t.Fatal(err)
	}
	// References must remain usable after the underlying path changes, and after
	// an ordinary runtime recreation. Neither operation copies the asset set.
	writeTestFile(t, filepath.Join(shared, "assets/untouched.txt"), "upstream changed text\n")
	commitShared("Update referenced text")
	nativeExec(t, d, sandbox, prefix+"test \"$(cat artwork/untouched.txt)\" = 'original unchanged text'")
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
	nativeExec(t, d, sandbox, prefix+"test -d renamed-empty/nested; test -f artwork/icon.bin; test \"$(cat artwork/untouched.txt)\" = 'original unchanged text'; rm artwork/3.bin; test ! -e artwork/3.bin; if rmdir artwork 2>/dev/null; then exit 1; fi")
	noAssetCopies()
	writeTestFile(t, filepath.Join(shared, "assets/conflict.txt"), "shared title\n")
	writeTestFile(t, filepath.Join(shared, "assets/nested/merge.txt"), "shared first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "assets/annotated.txt"), "shared heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n")
	commitShared("Concurrent edits at original paths")
	activate := func(exit int) ActivationResult {
		t.Helper()
		body := nativeExec(t, d, sandbox, "activate --json --message 'Directory rename'; status=$?; test \"$status\" -eq "+strconv.Itoa(exit))
		var result ActivationResult
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatalf("decode activation: %q: %v", body, err)
		}
		return result
	}
	first := activate(3)
	if first.Status != "conflicted" || len(first.Packages) != 1 {
		t.Fatalf("expected shared native conflict: %+v", first)
	}
	var attempt activationAttempt
	if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
		t.Fatal(err)
	}
	worktree := attempt.Packages[0].Worktree
	nativeExec(t, d, sandbox, "test ! -e "+shellQuote(worktree+"/artwork/icon.bin"))
	conflicts := nativeExec(t, d, sandbox, "git -C "+shellQuote(worktree)+" ls-files -u")
	if !strings.Contains(conflicts, "assets/conflict.txt") || !strings.Contains(conflicts, "artwork/annotated.txt") {
		t.Fatalf("missing renamed conflict index: %s", conflicts)
	}
	nativeExec(t, d, sandbox, "set -e; cd "+shellQuote(worktree)+"; test \"$(head -c 7 artwork/annotated.txt)\" = '<<<<<<<'; printf 'resolved title\n' >artwork/conflict.txt; printf 'resolved heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n' >artwork/annotated.txt; git rm assets/conflict.txt; git add artwork/conflict.txt artwork/annotated.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -m 'Resolve renamed file'")
	noAssetCopies()
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("expected one renamed candidate")
			}
			candidate := candidates[0].Root
			for _, pair := range [][2]string{{"assets/0.bin", "artwork/icon.bin"}, {"assets/1.bin", "artwork/1.bin"}, {"assets/2.bin", "artwork/2.bin"}} {
				a, err := os.Stat(filepath.Join(shared, pair[0]))
				if err != nil {
					return err
				}
				b, err := os.Stat(filepath.Join(candidate, pair[1]))
				if err != nil || !os.SameFile(a, b) {
					return fmt.Errorf("candidate copied renamed asset %s: %v", pair[1], err)
				}
			}
			_, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'later private edit\n' >artwork/nested/merge.txt; printf alive >/tmp/rename-process-marker")
			return err
		}, complete: func(context.Context, string, bool) error { return nil },
	})
	last := activate(0)
	if !last.Success {
		t.Fatalf("rename publication: %+v", last)
	}
	for name, want := range map[string]string{
		"artwork/conflict.txt":     "resolved title\n",
		"artwork/nested/merge.txt": "shared first\n2\n3\n4\n5\n6\n7\nprivate last\n",
		"artwork/untouched.txt":    "upstream changed text\n",
		"artwork/new.txt":          "new file\n",
		"artwork/after-rename.txt": "edited after rename\n",
	} {
		body, err := os.ReadFile(filepath.Join(shared, name))
		if err != nil || string(body) != want {
			t.Fatalf("published %s: %q, %v", name, body, err)
		}
	}
	for _, name := range []string{"assets/0.bin", "artwork/3.bin", "artwork/removed.txt"} {
		if _, err := os.Stat(filepath.Join(shared, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted path %s remains: %v", name, err)
		}
	}
	nativeExec(t, d, sandbox, prefix+"test \"$(cat artwork/nested/merge.txt)\" = 'later private edit'; test \"$(cat /tmp/rename-process-marker)\" = alive; test ! -e artwork/3.bin; test -f artwork/icon.bin")
	noAssetCopies()

}

func nativePackageRenames(t *testing.T) {
	m, sparse, sandbox, _ := nativeSparseRuntime(t, "moves", 1)
	ctx, d := context.Background(), sparse.RunscDriver
	m.SetSchemaDeployment(nativeActivationHook{prepare: func(context.Context, string, []deployment.Candidate) error { return nil }, complete: func(context.Context, string, bool) error { return nil }})
	for _, id := range []string{"rename/original", "oldspace/one", "oldspace/two"} {
		root := filepath.Join(sparse.shared, id)
		writeTestFile(t, filepath.Join(root, "package.toml"), "schema = 1\n")
		for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
			writeTestFile(t, filepath.Join(root, "folder", name), name+"\n")
		}
		initializeTestRepository(t, m, id, "Fixture", "fixture@example.test", "Package rename fixture")
	}
	original := filepath.Join(sparse.shared, "rename/original")
	writeTestFile(t, filepath.Join(original, "asset.bin"), strings.Repeat("asset\x00", 200000))
	if _, err := gitCommand(ctx, original, nil, "add", "asset.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, original, gitIdentity("Fixture", "fixture@example.test"), "commit", "-qm", "Add asset"); err != nil {
		t.Fatal(err)
	}
	asset, err := os.Stat(filepath.Join(original, "asset.bin"))
	if err != nil {
		t.Fatal(err)
	}
	privateHead := nativeExec(t, d, sandbox, "set -e; cd /workspace/packages/rename/original; printf 'private label\\n' >label.txt; git add label.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Private history'; git rev-parse HEAD")
	nativeExec(t, d, sandbox, "set -e; cd /workspace/packages; mv rename/original rename/intermediate; mv rename/intermediate rename/final; test ! -e rename/original; test ! -e rename/intermediate; test -f rename/final/asset.bin; test \"$(git -C rename/final rev-parse --show-toplevel)\" = /workspace/packages/rename/final; mv rename/final/folder rename/final/renamed-folder")
	for _, id := range []string{"rename/final", "rename/original"} {
		preview, err := m.Preview(ctx, sandbox.UserID, ActivationOptions{SelectedPackages: []string{id}, PreviewFile: "package.toml"})
		if err != nil || len(preview.Packages) != 1 {
			t.Fatalf("preview %s: %+v, %v", id, preview, err)
		}
		kind := "added"
		if id == "rename/original" {
			kind = "deleted"
		}
		if preview.Packages[0].Change != kind {
			t.Fatalf("package change: %+v", preview.Packages[0])
		}
		for _, file := range preview.Packages[0].Files {
			if file.Change != kind {
				t.Fatalf("file change for %s: %+v", id, file)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(sparse.storage, "upper/rename/final/asset.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename copied its asset: %v", err)
	}
	activate := func(ids ...string) ActivationResult {
		t.Helper()
		result, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Package lifecycle", SelectedPackages: ids})
		if err != nil || !result.Success {
			t.Fatalf("activate %v: %+v, %v", ids, result, err)
		}
		return result
	}
	result := activate("rename/final")
	if len(result.Packages) != 2 {
		t.Fatalf("package rename was not coupled: %+v", result)
	}
	if _, err := os.Stat(original); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old package remains: %v", err)
	}
	final := filepath.Join(sparse.shared, "rename/final")
	publishedAsset, err := os.Stat(filepath.Join(final, "asset.bin"))
	if err != nil || !os.SameFile(asset, publishedAsset) {
		t.Fatalf("activation copied renamed asset: %v", err)
	}
	if _, err := gitCommand(ctx, final, nil, "merge-base", "--is-ancestor", strings.TrimSpace(privateHead), "HEAD"); err != nil {
		t.Fatalf("lost private Git history: %v", err)
	}
	nativeExec(t, d, sandbox, "set -e; cd /workspace/packages; test ! -e rename/original; test -f rename/final/renamed-folder/one.txt; git -C rename/final status --porcelain >/dev/null; mv oldspace newspace; test ! -e oldspace; test -f newspace/one/package.toml; test -f newspace/two/package.toml")
	upstream := filepath.Join(sparse.shared, "oldspace/one")
	writeTestFile(t, filepath.Join(upstream, "folder/one.txt"), "Concurrent upstream edit\n")
	if _, err := gitCommand(ctx, upstream, gitIdentity("Fixture", "fixture@example.test"), "commit", "-qam", "Concurrent edit before namespace activation"); err != nil {
		t.Fatal(err)
	}
	conflicted, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Namespace rename", SelectedPackages: []string{"newspace/one"}})
	if err == nil || conflicted.Status != "conflicted" || len(conflicted.Packages) != 1 || !slices.Contains(conflicted.Packages[0].Conflicts, "folder/one.txt") {
		t.Fatalf("namespace rename lost upstream conflict: %+v, %v", conflicted, err)
	}
	worktree := conflicted.Packages[0].ConflictWorktree
	nativeExec(t, d, sandbox, "set -e; cd "+shellQuote(worktree)+"; git rm folder/one.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Confirm old package removal'")
	result = activate("newspace/one")
	if len(result.Packages) != 4 {
		t.Fatalf("namespace rename was not coupled: %+v", result)
	}
	for _, id := range []string{"newspace/one", "newspace/two"} {
		if _, err := os.Stat(filepath.Join(sparse.shared, id, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	// Removing the namespace must retire each package; ordinary CLI creation
	// remains activatable with neither a prior catalog entry nor git init.
	nativeExec(t, d, sandbox, "set -e; cd /workspace/packages; rm -r newspace; mkdir -p plain/new; printf 'schema = 1\\n' >plain/new/package.toml; printf 'new file\\n' >plain/new/label.txt; test ! -e plain/new/.git")
	activate("newspace/one", "newspace/two", "plain/new")
	for _, id := range []string{"newspace/one", "newspace/two"} {
		if _, err := os.Stat(filepath.Join(sparse.shared, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("package deletion %s: %v", id, err)
		}
	}
	if _, err := gitCommand(ctx, filepath.Join(sparse.shared, "plain/new"), nil, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	nativeExec(t, d, sandbox, "mv /workspace/packages/rename/final/label.txt /workspace/packages/plain/new/moved-label.txt")
	for _, change := range []struct{ id, name, kind string }{{"rename/final", "label.txt", "deleted"}, {"plain/new", "moved-label.txt", "added"}} {
		preview, err := m.Preview(ctx, sandbox.UserID, ActivationOptions{SelectedPackages: []string{change.id}, PreviewFile: change.name})
		if err != nil || len(preview.Packages) != 1 || len(preview.Packages[0].Files) != 1 || preview.Packages[0].Files[0].Change != change.kind {
			t.Fatalf("cross-package move preview: %+v, %v", preview, err)
		}
	}
	activate("rename/final", "plain/new")
	if _, err := os.Stat(filepath.Join(final, "label.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-package source remains: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(sparse.shared, "plain/new/moved-label.txt")); err != nil || string(body) != "private label\n" {
		t.Fatalf("cross-package destination: %q, %v", body, err)
	}
	nativeExec(t, d, sandbox, "set -e; cd /workspace/packages; mkdir -p plain/initialized; printf 'schema = 1\\n' >plain/initialized/package.toml; git -C plain/initialized init -q; git -C plain/initialized add package.toml; git -C plain/initialized -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Agent commit'")
	activate("plain/initialized")
	preview, err := m.Preview(ctx, sandbox.UserID, ActivationOptions{})
	if err != nil || len(preview.Packages) != 0 {
		t.Fatalf("clean lifecycle preview: %+v, %v", preview, err)
	}
	nativeExec(t, d, sandbox, "set -e; mkdir -p /workspace/packages/plain/invalid; printf bad >/workspace/packages/plain/invalid/source.txt")
	if _, err := m.Preview(ctx, sandbox.UserID, ActivationOptions{SelectedPackages: []string{"plain/invalid"}}); err == nil || !strings.Contains(err.Error(), "package.toml") {
		t.Fatalf("missing manifest must be actionable: %v", err)
	}
}
