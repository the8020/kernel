package development

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

type dropStagingReply struct {
	net.Conn
	pending, dropped bool
}

type committedLifecycleHook struct{ nativeActivationHook }

func (committedLifecycleHook) ForwardRequired(context.Context, string) (bool, error) {
	return true, nil
}

type lifecyclePlanningHook struct{ nativeActivationHook }

func (lifecyclePlanningHook) Plan(context.Context, []deployment.Candidate) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func TestActivationInventoryPreservesUnknownTrees(t *testing.T) {
	m := newTestPlatform(t).manager
	sandbox := Sandbox{UserID: "inventory", SandboxID: "sbx-0123456789"}
	d := &workspace{storage: t.TempDir()}
	activeID, legacyID := strings.Repeat("1", 24), strings.Repeat("2", 24)
	for _, id := range []string{activeID, legacyID} {
		writeTestFile(t, filepath.Join(d.storage, "upper/.conflicts", id, "unresolved.txt"), "keep this work")
	}
	attempt := activationAttempt{ID: activeID, SandboxID: sandbox.SandboxID, Phase: "captured"}
	journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
	if err := saveAttempt(journal, &attempt); err != nil {
		t.Fatal(err)
	}
	sandbox.LastActivationResult = &ActivationResult{AttemptID: activeID, Error: "Preserved worktree was edited"}
	status, err := m.activationStatus(sandbox, d)
	if err != nil || status.AttemptID != activeID || len(status.Retained) != 1 || status.Retained[0].Status != "legacy-unresolved" {
		t.Fatalf("inventory: %+v %v", status, err)
	}
	if !strings.Contains(status.Error, "Preserved worktree was edited") {
		t.Fatal("status lost the pending attempt's diagnostic")
	}
	if err := m.retainAttempt(sandbox, journal, &attempt); err != nil {
		t.Fatal(err)
	}
	status, err = m.activationStatus(sandbox, d)
	if err != nil || status.AttemptID != "" || len(status.Retained) != 2 || status.Retained[0].Status != "aborted-preserved" {
		t.Fatalf("retained inventory: %+v %v", status, err)
	}
	partial := activationAttempt{ID: strings.Repeat("3", 24), SandboxID: sandbox.SandboxID, Phase: "cleanup"}
	if err := saveAttempt(journal, &partial); err != nil {
		t.Fatal(err)
	}
	if err := m.retainAttempt(sandbox, journal, &partial); err != nil {
		t.Fatal(err)
	}
	status, err = m.activationStatus(sandbox, d)
	if err != nil || len(status.Retained) != 3 || status.Retained[2].Recovery == "" {
		t.Fatalf("hidden partial retirement disappeared from inventory: %+v %v", status, err)
	}
	for i := 0; i < 130; i++ {
		writeTestFile(t, filepath.Join(d.storage, "upper/.conflicts", fmt.Sprintf("%024x", i+100), "file"), "legacy")
	}
	status, err = m.activationStatus(sandbox, d)
	if err != nil || !status.InventoryTruncated || len(status.Retained) > 128 {
		t.Fatalf("inventory is unbounded: %+v %v", status, err)
	}
	for _, id := range []string{activeID, legacyID} {
		if body, err := os.ReadFile(filepath.Join(d.storage, "upper/.conflicts", id, "unresolved.txt")); err != nil || string(body) != "keep this work" {
			t.Fatalf("inventory changed retained work: %q %v", body, err)
		}
	}
}

func TestSourceResetClearsDiscardedRetainedInventory(t *testing.T) {
	m := newTestPlatform(t).manager
	sandbox := Sandbox{UserID: "resetinventory", SandboxID: "sbx-0123456789"}
	root := m.sandboxRoot(sandbox)
	writeTestFile(t, filepath.Join(root, "workspace/upper/.conflicts/old/file"), "discarded only by confirmed source reset")
	writeTestFile(t, filepath.Join(root, "activation/retained/old.json"), "retained record")
	writeTestFile(t, filepath.Join(root, "home-marker"), "preserve")
	if err := m.resetWorkspace(&sandbox); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workspace", "activation/retained"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reset retained stale %s: %v", name, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(root, "home-marker")); err != nil || string(body) != "preserve" {
		t.Fatalf("source reset affected unrelated state: %q %v", body, err)
	}
}

func (c *dropStagingReply) Write(data []byte) (int, error) {
	if !c.dropped && strings.Contains(string(data), `"action":"retire-staging"`) {
		c.pending = true
	}
	return c.Conn.Write(data)
}

func (c *dropStagingReply) Read(data []byte) (int, error) {
	if c.pending {
		c.pending, c.dropped = false, true
		var result map[string]any
		if err := json.NewDecoder(io.LimitReader(c.Conn, 4<<20)).Decode(&result); err != nil {
			return 0, err
		}
		return 0, errors.New("injected interrupted staging retirement")
	}
	return c.Conn.Read(data)
}

func TestNativeActivationLifecycle(t *testing.T) {
	m, _, shared := nativeRuntime(t)
	ctx := context.Background()
	sandbox, err := m.Create(ctx, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
	d, ok := m.workspaceFor(sandbox.SandboxID)
	if !ok {
		t.Fatal("missing native workspace")
	}
	journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
	load := func() activationAttempt {
		t.Helper()
		var attempt activationAttempt
		if err := readJSON(journal, &attempt); err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	var beforeComplete func()
	m.SetSchemaDeployment(committedLifecycleHook{nativeActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error {
			attempt := load()
			item := attempt.Packages[0]
			nativeExec(t, d.RunscDriver, sandbox, "test ! -e "+shellQuote(item.Worktree+"/untouched.txt"))
			return nil
		},
		complete: func(_ context.Context, _ string, activated bool) error {
			if activated && beforeComplete != nil {
				beforeComplete()
			}
			return nil
		},
	}})
	activate := func(action string) (ActivationResult, error) {
		return m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: action, Description: "Lifecycle fixture", AuthorName: "Jan Tomášek", AuthorEmail: "tomasekhonza@gmail.com"})
	}
	edit := func(body string) {
		t.Helper()
		nativeExec(t, d.RunscDriver, sandbox, "printf '%s\\n' "+shellQuote(body)+" >/workspace/packages/the8020/dev-core/same.txt")
	}
	assertRetired := func(attempt activationAttempt) {
		t.Helper()
		for _, item := range attempt.Packages {
			nativeExec(t, d.RunscDriver, sandbox, "test ! -e "+shellQuote(item.Worktree)+"; test ! -e "+shellQuote(item.GitDir))
			if _, err := os.Lstat(filepath.Join(d.storage, "upper", strings.TrimPrefix(item.Worktree, "/workspace/packages/"))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging disk entry remains: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(d.storage, "git", strings.TrimPrefix(item.GitDir, "/workspace/git/private/"))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Git registration remains: %v", err)
			}
			if _, err := os.Stat(filepath.Join(d.storage, "snapshots", stagingID(attempt.ID, item.PackageID), "staging")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retirement payload remains: %v", err)
			}
		}
		if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("active journal remains: %v", err)
		}
		entries, err := os.ReadDir(filepath.Dir(journal))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "validation-") {
				t.Fatalf("completed validation staging remains: %s", entry.Name())
			}
		}
	}
	for i := 0; i < 2; i++ {
		edit(fmt.Sprintf("successful %d", i))
		var attempt activationAttempt
		beforeComplete = func() { attempt = load() }
		result, err := activate("")
		if err != nil || !result.Success {
			t.Fatalf("successful activation: %+v: %v", result, err)
		}
		assertRetired(attempt)
	}
	lifecycleHook := m.schemaDeployment()
	for _, phase := range []string{"preparing", "published"} {
		edit("recovered " + phase)
		fail := true
		var recovered activationAttempt
		m.SetSchemaDeployment(committedLifecycleHook{nativeActivationHook{
			prepare: func(context.Context, string, []deployment.Candidate) error {
				recovered = load()
				if phase == "preparing" && fail {
					return errors.New("lost committed preparation reply")
				}
				return nil
			},
			complete: func(context.Context, string, bool) error {
				if phase == "published" && fail {
					return errors.New("interrupted finalization")
				}
				return nil
			},
		}})
		result, err := activate("")
		if err == nil || load().Phase != phase {
			t.Fatalf("missing %s recovery: %+v %v", phase, result, err)
		}
		if _, err := os.Stat(recovered.Validation); err != nil {
			t.Fatal("lost validation before recovery", err)
		}
		if _, err := m.Stop(ctx, sandbox.UserID); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Start(ctx, sandbox.UserID); err != nil {
			t.Fatal(err)
		}
		d, _ = m.workspaceFor(sandbox.SandboxID)
		fail = false
		result, err = activate("continue")
		if err != nil || !result.Success {
			t.Fatalf("resumed %s: %+v %v", phase, result, err)
		}
		assertRetired(recovered)
	}
	m.SetSchemaDeployment(lifecycleHook)
	edit("interrupted cleanup")
	var interrupted activationAttempt
	beforeComplete = func() { interrupted = load(); d.control = &dropStagingReply{Conn: d.control}; beforeComplete = nil }
	result, err := activate("")
	if err == nil || result.Status != "cleanup-pending" || load().Phase != "cleanup" {
		t.Fatalf("interruption not retained: %+v: %v", result, err)
	}
	if _, err := m.Stop(ctx, sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	d, _ = m.workspaceFor(sandbox.SandboxID)
	status, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: "status"})
	if err != nil || status.Status != "cleanup-pending" {
		t.Fatalf("restart lost cleanup: %+v: %v", status, err)
	}
	var helperStatus ActivationResult
	if err := json.Unmarshal([]byte(nativeExec(t, d.RunscDriver, sandbox, "activate --status --json")), &helperStatus); err != nil || helperStatus.Status != "cleanup-pending" {
		t.Fatalf("helper lost pending cleanup: %+v %v", helperStatus, err)
	}
	result, err = activate("continue")
	if err != nil || !result.Success {
		t.Fatalf("cleanup retry: %+v: %v", result, err)
	}
	assertRetired(interrupted)
	edit("published before later work")
	var changed activationAttempt
	beforeComplete = func() {
		changed = load()
		nativeExec(t, d.RunscDriver, sandbox, "printf 'later resolution\\n' >"+shellQuote(changed.Packages[0].Worktree+"/same.txt"))
		beforeComplete = nil
	}
	result, err = activate("")
	if err == nil || result.Status != "cleanup-pending" {
		t.Fatalf("later edit removed: %+v: %v", result, err)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "published before later work\n" {
		t.Fatalf("published wrong bytes: %q %v", body, err)
	}
	// The same user's shell and a different user's activation remain live.
	nativeExec(t, d.RunscDriver, sandbox, "printf 'unrelated shell alive\\n'")
	peer, err := m.Create(ctx, "lifecyclepeer")
	if err != nil {
		t.Fatal(err)
	}
	peerD, _ := m.workspaceFor(peer.SandboxID)
	nativeExec(t, peerD.RunscDriver, peer, "printf peer >/workspace/packages/the8020/dev-core/disjoint.txt")
	beforeComplete = nil
	m.SetSchemaDeployment(nativeActivationHook{prepare: func(context.Context, string, []deployment.Candidate) error { return nil }, complete: func(context.Context, string, bool) error { return nil }})
	peerResult, err := m.Activate(ctx, peer.UserID, ActivationOptions{Description: "Independent user", AuthorName: "Jan Tomášek", AuthorEmail: "tomasekhonza@gmail.com"})
	if err != nil || !peerResult.Success {
		t.Fatalf("unrelated activation blocked: %+v %v", peerResult, err)
	}
	if err := m.Delete(ctx, peer.UserID); err != nil {
		t.Fatal(err)
	}
	result, err = activate("abort")
	if err != nil || !result.Success || result.Status != "aborted-preserved" {
		t.Fatalf("safe abort: %+v %v", result, err)
	}
	nativeExec(t, d.RunscDriver, sandbox, "test \"$(cat "+shellQuote(changed.Packages[0].Worktree+"/same.txt")+")\" = 'later resolution'")
	for _, scenario := range []string{"new-commit", "open-handle", "locked-registration"} {
		edit("publication before " + scenario)
		var retained activationAttempt
		m.SetSchemaDeployment(nativeActivationHook{
			prepare: func(context.Context, string, []deployment.Candidate) error { return nil },
			complete: func(_ context.Context, _ string, activated bool) error {
				if !activated {
					return nil
				}
				retained = load()
				wt := retained.Packages[0].Worktree
				switch scenario {
				case "new-commit":
					nativeExec(t, d.RunscDriver, sandbox, "cd "+shellQuote(wt)+"; printf 'unpublished commit\\n' >same.txt; git add same.txt; git -c user.name='Jan Tomášek' -c user.email=tomasekhonza@gmail.com commit -m 'Unpublished later work'")
				case "open-handle":
					nativeExec(t, d.RunscDriver, sandbox, "bash -c "+shellQuote("exec 9<>"+shellQuote(wt+"/same.txt")+"; echo $$ >/tmp/staging-holder.pid; exec sleep 60")+" </dev/null >/tmp/staging-holder.log 2>&1 &")
					nativeExec(t, d.RunscDriver, sandbox, "for i in $(seq 1 50); do test ! -e /tmp/staging-holder.pid || exit 0; sleep .02; done; exit 1")
				case "locked-registration":
					nativeExec(t, d.RunscDriver, sandbox, "git --git-dir=/workspace/git/private/the8020/dev-core/.git worktree lock "+shellQuote(wt))
				}
				return nil
			},
		})
		result, err = activate("")
		if err == nil || result.Status != "cleanup-pending" {
			t.Fatalf("%s cleanup ignored a concurrent change: %+v %v", scenario, result, err)
		}
		item := retained.Packages[0]
		switch scenario {
		case "new-commit":
			nativeExec(t, d.RunscDriver, sandbox, "cd "+shellQuote(item.Worktree)+"; test -z \"$(git status --porcelain)\"; test \"$(cat same.txt)\" = 'unpublished commit'")
			result, err = activate("abort")
		case "open-handle":
			nativeExec(t, d.RunscDriver, sandbox, "test -f "+shellQuote(item.Worktree+"/same.txt")+"; kill $(cat /tmp/staging-holder.pid); rm /tmp/staging-holder.pid")
			result, err = activate("continue")
		case "locked-registration":
			if _, err := os.Stat(filepath.Join(d.storage, "snapshots", stagingID(retained.ID, item.PackageID), "staging/same.txt")); err != nil {
				t.Fatalf("partial cleanup lost protected file: %v", err)
			}
			nativeExec(t, d.RunscDriver, sandbox, "git --git-dir=/workspace/git/private/the8020/dev-core/.git worktree unlock "+shellQuote(item.Worktree))
			result, err = activate("continue")
		}
		if err != nil || !result.Success {
			t.Fatalf("%s recovery: %+v %v", scenario, result, err)
		}
		if scenario != "new-commit" {
			assertRetired(retained)
		}
	}
	m.SetSchemaDeployment(nativeActivationHook{prepare: func(context.Context, string, []deployment.Candidate) error { return nil }, complete: func(context.Context, string, bool) error { return nil }})
	legacy := strings.Repeat("b", 24)
	nativeExec(t, d.RunscDriver, sandbox, "mkdir -p /workspace/packages/.conflicts/"+legacy+"; printf unknown >/workspace/packages/.conflicts/"+legacy+"/work")
	edit("after legacy")
	result, err = activate("")
	if err == nil || !strings.Contains(err.Error(), "unknown recovery ownership") {
		t.Fatalf("unknown legacy not gated: %+v %v", result, err)
	}
	result, err = m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: "abort", AttemptID: legacy})
	if err != nil || !result.Success {
		t.Fatalf("legacy disposition: %+v %v", result, err)
	}
	nativeExec(t, d.RunscDriver, sandbox, "test \"$(cat /workspace/packages/.conflicts/"+legacy+"/work)\" = unknown")
	result, err = activate("")
	if err != nil || !result.Success {
		t.Fatalf("activation after explicit disposition: %+v %v", result, err)
	}
	// Only actual unmerged entries are conflicts; continuing cannot skip them.
	edit("private conflicting change")
	writeTestFile(t, filepath.Join(shared, "same.txt"), "shared conflicting change\n")
	if _, err := gitCommand(ctx, shared, gitIdentity("Jan Tomášek", "tomasekhonza@gmail.com"), "commit", "-am", "Conflict fixture"); err != nil {
		t.Fatal(err)
	}
	result, err = activate("")
	if err == nil || result.Status != "conflicted" || len(result.Packages[0].Conflicts) == 0 {
		t.Fatalf("conflict not explicit: %+v %v", result, err)
	}
	conflict := load()
	preview, previewErr := m.Preview(ctx, sandbox.UserID, ActivationOptions{})
	if previewErr != nil || preview.Pending == nil || preview.Pending.AttemptID != conflict.ID || len(preview.Packages) != 0 {
		t.Fatalf("pending preview did not retain the captured attempt: %+v %v", preview, previewErr)
	}
	result, err = activate("continue")
	if err == nil || result.Status != "conflicted" {
		t.Fatalf("continued unresolved conflict: %+v %v", result, err)
	}
	wt := conflict.Packages[0].Worktree
	nativeExec(t, d.RunscDriver, sandbox, "cd "+shellQuote(wt)+"; printf 'resolved\\n' >same.txt; git add same.txt; git -c user.name='Jan Tomášek' -c user.email=tomasekhonza@gmail.com commit -m resolution")
	result, err = activate("continue")
	if err != nil || !result.Success {
		t.Fatalf("resolved continuation: %+v %v", result, err)
	}
	// GitDir is assigned after successful resolution, so check registration via Git.
	nativeExec(t, d.RunscDriver, sandbox, "test ! -e "+shellQuote(wt)+"; ! git --git-dir=/workspace/git/private/the8020/dev-core/.git worktree list --porcelain | grep -F "+shellQuote(wt))
}

func TestValidationCleanupRetainsJournalOnFailure(t *testing.T) {
	m := newTestPlatform(t).manager
	sandbox := Sandbox{UserID: "validation", SandboxID: "sbx-0123456789"}
	journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
	outside := filepath.Join(t.TempDir(), "validation-foreign")
	writeTestFile(t, filepath.Join(outside, "keep"), "foreign")
	attempt := activationAttempt{ID: strings.Repeat("a", 24), SandboxID: sandbox.SandboxID, Phase: "cleanup", Validation: outside}
	if err := saveAttempt(journal, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := m.retireStaging(context.Background(), sandbox, nil, journal, &attempt); err == nil {
		t.Fatal("removed validation staging outside the owned activation directory")
	}
	if err := readJSON(journal, &attempt); err != nil {
		t.Fatal("lost retry journal", err)
	}
	stage := filepath.Join(filepath.Dir(journal), "validation-saved")
	writeTestFile(t, filepath.Join(stage, "team/pkg/file"), "candidate")
	attempt.Validation = ""
	attempt.Candidates = []deployment.Candidate{{PackageID: "team/pkg", Root: filepath.Join(stage, "team/pkg")}}
	if err := saveAttempt(journal, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(journal, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := m.retireStaging(context.Background(), sandbox, nil, journal, &attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saved validation remains: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(body) != "foreign" {
		t.Fatal("changed foreign files", err)
	}
}

func TestNativeActivationRetryCleanup(t *testing.T) {
	m, d, sandbox, _ := nativeSparseRuntime(t, "retrycleanup", 1)
	ctx := context.Background()
	journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
	nativeExec(t, d.RunscDriver, sandbox, "printf 'retry source\n' >/workspace/packages/the8020/dev-core/same.txt")
	var pending activationAttempt
	reject := true
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error {
			if err := readJSON(journal, &pending); err != nil {
				return err
			}
			if reject {
				return errors.New("validation rejected")
			}
			return nil
		},
		complete: func(context.Context, string, bool) error { return nil },
	})
	activate := func() (ActivationResult, error) {
		return m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Retry cleanup", AuthorName: "Jan Tomášek", AuthorEmail: "tomasekhonza@gmail.com"})
	}
	for i := 0; i < 4; i++ {
		if i == 3 {
			reject = false
		}
		result, err := activate()
		if reject && (err == nil || result.Success) || !reject && (err != nil || !result.Success) {
			t.Fatalf("retry %d: %+v %v", i, result, err)
		}
		if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry retained journal: %v", err)
		}
		for _, item := range pending.Packages {
			nativeExec(t, d.RunscDriver, sandbox, "test ! -e "+shellQuote(item.Worktree)+"; test ! -e "+shellQuote(item.GitDir))
			for _, capture := range item.Captures {
				for _, payload := range []string{"file", "base", "file-reference", "base-reference"} {
					if _, err := os.Lstat(filepath.Join(d.storage, "snapshots", capture.ID, payload)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("retry retained capture %s: %v", payload, err)
					}
				}
			}
		}
		if _, err := os.Stat(pending.Validation); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry retained validation: %v", err)
		}
		status, err := m.activationStatus(sandbox, d)
		if err != nil || len(status.Retained) != 0 || status.Status != "idle" {
			t.Fatalf("retry hoarded attempts: %+v %v", status, err)
		}
		if got := nativeExec(t, d.RunscDriver, sandbox, "cat /workspace/packages/the8020/dev-core/same.txt"); got != "retry source\n" {
			t.Fatalf("retry changed private source: %q", got)
		}
	}

	// A lost retirement reply leaves one cleanup journal, never a new capture.
	nativeExec(t, d.RunscDriver, sandbox, "printf 'after success\n' >/workspace/packages/the8020/dev-core/same.txt")
	interrupt, blockRollback := true, false
	m.SetSchemaDeployment(nativeActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error { return errors.New("rejected") },
		complete: func(_ context.Context, _ string, published bool) error {
			if blockRollback {
				return errors.New("rollback unavailable")
			}
			if !published && interrupt {
				d.control = &dropStagingReply{Conn: d.control}
				interrupt = false
			}
			return nil
		},
	})
	result, err := activate()
	if err == nil || result.Phase != "discarding" {
		t.Fatalf("lost rollback retirement: %+v %v", result, err)
	}
	if _, err := m.Stop(ctx, sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	d, _ = m.workspaceFor(sandbox.SandboxID)
	result, err = m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: "continue"})
	if err != nil || !result.Success || result.Status != "not-committed" {
		t.Fatalf("rollback cleanup retry: %+v %v", result, err)
	}
	for i := 0; i < 2; i++ {
		blockRollback = true
		result, err = activate()
		if err == nil || result.Phase != "preparing" {
			t.Fatalf("rollback fixture: %+v %v", result, err)
		}
		blockRollback = false
		result, err = m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: "abort"})
		if err != nil || !result.Success {
			t.Fatalf("safe abort: %+v %v", result, err)
		}
		status, err := m.activationStatus(sandbox, d)
		if err != nil || len(status.Retained) != 0 || status.Status != "idle" {
			t.Fatalf("abort hoarded generated data: %+v %v", status, err)
		}
	}

	// A clean user commit after preview is distinct work, even without a conflict.
	m.SetSchemaDeployment(lifecyclePlanningHook{nativeActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error {
			return errors.New("validation rejected custom staging")
		},
		complete: func(context.Context, string, bool) error { return nil },
	}})
	preview, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "Preview", SchemaPreview: true})
	if err != nil || !preview.Success {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if err := readJSON(journal, &pending); err != nil {
		t.Fatal(err)
	}
	wt := pending.Packages[0].Worktree
	nativeExec(t, d.RunscDriver, sandbox, "cd "+shellQuote(wt)+"; printf 'custom staging\n' >same.txt; git add same.txt; git -c user.name='Jan Tomášek' -c user.email=tomasekhonza@gmail.com commit -m 'Custom staging'")
	for i := 0; i < 2; i++ {
		result, err = activate()
		if err == nil || result.AttemptID != pending.ID || result.Phase != "captured" {
			t.Fatalf("custom staging retry: %+v %v", result, err)
		}
		if got := nativeExec(t, d.RunscDriver, sandbox, "cat "+shellQuote(wt+"/same.txt")); got != "custom staging\n" {
			t.Fatalf("lost custom staging: %q", got)
		}
	}
	result, err = m.Activate(ctx, sandbox.UserID, ActivationOptions{Action: "abort"})
	if err != nil || !result.Success || result.Status != "aborted-preserved" {
		t.Fatalf("custom staging abort: %+v %v", result, err)
	}
}
