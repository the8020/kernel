package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
)

func TestOrdinarySynchronizationPreparesCompleteSet(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires Deno for cyclic imports")
	}
	url, commits := synchronizationRemotes(t)
	for _, order := range [][]string{{"alpha", "beta"}, {"beta", "alpha"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			root, store, db := activationStore(t)
			for _, name := range order {
				if _, err := store.SetPackageIndex(t.Context(), PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git", Commit: commits[name]}); err != nil {
					t.Fatal(err)
				}
			}
			preparations := 0
			schema := &recordingSchemaHook{inspect: func(candidates []deployment.Candidate) error {
				preparations++
				if len(candidates) != 2 {
					return fmt.Errorf("incomplete selected set: %d", len(candidates))
				}
				return nil
			}}
			calls := map[string]int{}
			runner := activationRunFunc(func(ctx context.Context, id, _ string, options jobs.Options) (jobs.Record, error) {
				calls[id]++
				imports := map[string]string{}
				for _, name := range order {
					imports["/p/acme/"+name+"/"] = "file://" + filepath.Join(root, "packages/acme", name) + "/"
				}
				for _, mount := range options.Mounts {
					imports["/p/"+mount.OwnerScope+"/"] = "file://" + mount.Source + "/"
				}
				encoded, _ := json.Marshal(map[string]any{"imports": imports})
				mapping := filepath.Join(root, "imports.json")
				writeFile(t, mapping, string(encoded))
				cmd := exec.CommandContext(ctx, deno, "eval", "--no-config", "--import-map", mapping, "if ((await import('/p/acme/alpha/mod.ts')).result() !== 'alpha-beta') throw Error('cycle');")
				if output, err := cmd.CombinedOutput(); err != nil {
					return jobs.Record{}, fmt.Errorf("%s: %s: %w", id, output, err)
				}
				return jobs.Record{State: "SUCCEEDED"}, nil
			})
			coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSchemaDeployment(coordinator)
			ids := []string{"acme/" + order[0], "acme/" + order[1]}
			results, err := store.SynchronizePackages(t.Context(), ids)
			if err != nil || len(results) != 2 || !results[0].Success || !results[1].Success {
				t.Fatalf("synchronize: %+v, %v", results, err)
			}
			for _, name := range order {
				for _, phase := range []string{"pre-activate", "post-activate"} {
					if calls["acme/"+name+"/"+phase] != 1 {
						t.Fatalf("hook cardinality: %v", calls)
					}
				}
			}
			if preparations != 1 {
				t.Fatalf("schema preparations: %v", schema.prepared)
			}
			results, err = store.SynchronizePackages(t.Context(), ids)
			if err != nil || !results[0].Success || !results[1].Success || preparations != 1 {
				t.Fatalf("no-op repeated activation: %+v %v %v", results, err, calls)
			}
			for id, count := range calls {
				if count != 1 {
					t.Fatalf("no-op repeated %s: %d", id, count)
				}
			}
			for _, name := range order {
				if _, err := os.Stat(filepath.Join(root, "packages/acme", name+".previous")); !os.IsNotExist(err) {
					t.Fatalf("backup remained: %v", err)
				}
			}
		})
	}
}

func TestOrdinarySynchronizationPublishedPeers(t *testing.T) {
	ctx, db := t.Context(), packageDatabase(t)
	url, commits := synchronizationRemotes(t)
	events := []string{}
	node := func(id string) *Store {
		store, err := New(Config{WorkspaceRoot: t.TempDir(), NodeID: id, Database: db})
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Packages: store, Schema: &activationSchemaRecorder{events: &events},
			Jobs: &activationJobRecorder{events: &events, calls: map[string]int{}}, Reindex: func(ctx context.Context, ids []string) error {
				_, err := store.ReindexHandlers(ctx, ids...)
				return err
			}})
		if err != nil {
			t.Fatal(err)
		}
		store.SetSchemaDeployment(coordinator)
		return store
	}
	publisher, receiver := node("publisher"), node("receiver")
	for _, name := range []string{"aconsumer", "beta", "missingref"} {
		if _, err := publisher.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git", Commit: commits[name]}); err != nil {
			t.Fatal(err)
		}
	}
	sync := func(store *Store, success bool, ids ...string) {
		t.Helper()
		results, err := store.SynchronizePackages(ctx, ids)
		if err != nil || len(results) != len(ids) {
			t.Fatal(results, err)
		}
		for _, result := range results {
			if result.Success != success {
				t.Fatal(results)
			}
		}
	}
	sync(publisher, true, "acme/beta")
	events = nil
	sync(receiver, false, "acme/missingref", "acme/beta")
	_, ready, err := receiver.runtimeIndex.Published(ctx)
	if _, statErr := os.Stat(receiver.packagePath("acme/beta")); !os.IsNotExist(statErr) || err != nil || len(ready) != 0 || len(events) != 0 {
		t.Fatalf("rejected selection changed checkout, receipts or effects: %v %v %v %v", statErr, ready, events, err)
	}
	sync(receiver, true, "acme/aconsumer", "acme/beta")
	if strings.Contains(strings.Join(events, ","), "acme/beta/") || len(receiver.EventListeners("sample")) != 1 {
		t.Fatalf("mixed activation replayed provider or lost listener: %v", events)
	}
	cold := node("cold")
	events = nil
	coordinator := cold.schemaDeployment().(*ActivationCoordinator)
	coordinator.validateCandidates = func(_ context.Context, candidates []deployment.Candidate) error {
		if len(candidates) != 2 || candidates[1].PackageID != "acme/beta" {
			t.Fatalf("expected complete switch set: %v", candidates)
		}
		return os.Rename(candidates[1].Root, candidates[1].Root+".unavailable")
	}
	sync(cold, false, "acme/beta", "acme/aconsumer")
	coordinator.validateCandidates = nil
	_, ready, err = cold.runtimeIndex.Published(ctx)
	if _, statErr := os.Stat(cold.packagePath("acme/aconsumer")); !os.IsNotExist(statErr) || err != nil || len(ready) != 0 {
		t.Fatalf("partial switch left source or receipts: %v %v %v", statErr, ready, err)
	}
	sync(cold, true, "acme/beta", "acme/aconsumer")
	revision, ready, err := cold.runtimeIndex.Published(ctx)
	if err != nil || revision != 1 || ready["acme/aconsumer"] != commits["aconsumer"] || ready["acme/beta"] != commits["beta"] || len(cold.EventListeners("sample")) != 1 || len(events) != 0 {
		t.Fatalf("catch-up must publish and index both receipts once, without system effects: %d %v %v %v", revision, ready, events, err)
	}
}

func TestOrdinarySynchronizationStagesBeforePublication(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	for _, peer := range []string{"missing", "invalid", "missingref"} {
		t.Run(peer, func(t *testing.T) {
			root, store, db := activationStore(t)
			events := []string{}
			coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
				Jobs: &activationJobRecorder{events: &events, calls: map[string]int{}, failures: map[string]int{}}})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSchemaDeployment(coordinator)
			for _, name := range []string{"alpha", peer} {
				if _, err := store.SetPackageIndex(t.Context(), PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git", Commit: commits[name]}); err != nil {
					t.Fatal(err)
				}
			}
			results, err := store.SynchronizePackages(t.Context(), []string{"acme/alpha", "acme/" + peer})
			if err != nil || len(results) != 2 || results[0].Success || results[1].Success || len(events) != 0 {
				t.Fatalf("partial acceptance: %+v %v %v", results, err, events)
			}
			if _, err := os.Stat(filepath.Join(root, "packages/acme/alpha")); !os.IsNotExist(err) {
				t.Fatalf("source published before complete validation: %v", err)
			}
		})
	}
}

func TestOrdinarySynchronizationPreservesCleanAheadBranch(t *testing.T) {
	ctx := t.Context()
	url, _ := synchronizationRemotes(t)
	_, store, _ := activationStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git"}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := store.SynchronizePackages(ctx, []string{"acme/beta"})
	if err != nil || len(results) != 1 || !results[0].Success {
		t.Fatal(results, err)
	}
	path := store.packagePath("acme/beta")
	writeFile(t, filepath.Join(path, "draft.txt"), "unpublished work\n")
	runTestGit(t, "git", path, "add", "draft.txt")
	runTestGit(t, "git", path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "unpublished work")
	ahead := runTestGit(t, "git", path, "rev-parse", "HEAD")
	if runTestGit(t, "git", path, "status", "--porcelain") != "" || runTestGit(t, "git", path, "rev-list", "--count", "@{upstream}..HEAD") != "1" {
		t.Fatal("expected a clean branch one commit ahead")
	}
	results, err = store.SynchronizePackages(ctx, []string{"acme/alpha", "acme/beta"})
	if err != nil || len(results) != 2 || results[0].Success || results[1].Success || !strings.Contains(results[1].Error, "not a fast-forward") {
		t.Fatal(results, err)
	}
	if runTestGit(t, "git", path, "rev-parse", "refs/heads/main") != ahead || runTestGit(t, "git", path, "status", "--porcelain") != "" {
		t.Fatal("rejected batch displaced the local commit or changed the worktree")
	}
	if _, err := os.Stat(store.packagePath("acme/alpha")); !os.IsNotExist(err) {
		t.Fatalf("rejected batch installed its first member: %v", err)
	}
	upstream := runTestGit(t, "git", path, "rev-parse", "@{upstream}")
	if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: "beta", Source: url + "/acme/beta.git", Commit: upstream}); err != nil {
		t.Fatal(err)
	}
	results, err = store.SynchronizePackages(ctx, []string{"acme/alpha", "acme/beta"})
	if err != nil || len(results) != 2 || results[0].Success || results[1].Success || !strings.Contains(results[1].Error, "not a fast-forward") || runTestGit(t, "git", path, "rev-parse", "HEAD") != ahead {
		t.Fatalf("a saved pin must not authorize discarding unpublished work: %+v %v", results, err)
	}
	runTestGit(t, "git", path, "checkout", "--detach", upstream)
	results, err = store.SynchronizePackages(ctx, []string{"acme/alpha", "acme/beta"})
	if err != nil || len(results) != 2 || results[0].Success || results[1].Success || !strings.Contains(results[1].Error, "not a fast-forward") {
		t.Fatalf("matching detached HEAD must not authorize resetting the default branch: %+v %v", results, err)
	}
	if runTestGit(t, "git", path, "branch", "--show-current") != "" || runTestGit(t, "git", path, "rev-parse", "refs/heads/main") != ahead {
		t.Fatal("rejected batch changed detached checkout or unpublished branch")
	}
	if _, err := os.Stat(store.packagePath("acme/alpha")); !os.IsNotExist(err) {
		t.Fatalf("rejected batch installed its first member: %v", err)
	}
}

func TestExplicitSyncRestoresDisplacedCheckout(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprint("dirty=", dirty), func(t *testing.T) {
			ctx := WithCheckoutRecovery(t.Context())
			url, _ := synchronizationRemotes(t)
			root, store, _ := activationStore(t)
			for _, name := range []string{"alpha", "beta"} {
				if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git"}); err != nil {
					t.Fatal(err)
				}
			}
			run := func(ids ...string) []PackageSynchronization {
				t.Helper()
				results, err := store.SynchronizePackages(ctx, ids)
				if err != nil || len(results) != len(ids) {
					t.Fatal(results, err)
				}
				return results
			}
			if result := run("acme/beta")[0]; !result.Success {
				t.Fatal(result)
			}
			path := store.packagePath("acme/beta")
			target := runTestGit(t, "git", path, "rev-parse", "HEAD")
			if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: "beta", Source: url + "/acme/beta.git", Commit: target}); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(path, "draft"), "commit witness")
			runTestGit(t, "git", path, "remote", "set-url", "origin", "https://example.invalid/private.git")
			runTestGit(t, "git", path, "add", "draft")
			runTestGit(t, "git", path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "unpublished")
			draft := runTestGit(t, "git", path, "rev-parse", "HEAD")
			if !dirty {
				runTestGit(t, "git", path, "branch", "local-topic", draft)
			}
			if dirty {
				runTestGit(t, "git", path, "checkout", "--detach", target)
				writeFile(t, filepath.Join(path, "package.toml"), "tracked witness")
				writeFile(t, filepath.Join(path, "notes"), "untracked witness")
			}
			checkOriginal := func(dir string) {
				t.Helper()
				if !dirty && (runTestGit(t, "git", dir, "symbolic-ref", "--short", "HEAD") != "main" || runTestGit(t, "git", dir, "rev-parse", "HEAD") != draft) {
					t.Fatal("unpublished checkout was not restored")
				}
				if runTestGit(t, "git", dir, "remote", "get-url", "origin") != "https://example.invalid/private.git" {
					t.Fatal("changed configured upstream")
				}
				if runTestGit(t, "git", dir, "rev-parse", "main") != draft || runTestGit(t, "git", dir, "show", "main:draft") != "commit witness" {
					t.Fatal("lost unpublished commit")
				}
				if dirty {
					for file, want := range map[string]string{"package.toml": "tracked witness", "notes": "untracked witness"} {
						data, err := os.ReadFile(filepath.Join(dir, file))
						if err != nil || string(data) != want {
							t.Fatal(file, err)
						}
					}
				}
			}
			recovery := filepath.Join(root, "checkout-recovery")
			writeFile(t, recovery, "not a directory")
			failed := run("acme/alpha", "acme/beta")
			if failed[0].Success || failed[1].Success || !strings.Contains(failed[1].Error, "recovery") {
				t.Fatal(failed)
			}
			checkOriginal(path)
			if _, err := os.Stat(store.packagePath("acme/alpha")); !os.IsNotExist(err) {
				t.Fatal("partial switch", err)
			}
			if err := os.Remove(recovery); err != nil {
				t.Fatal(err)
			}
			results := run("acme/alpha", "acme/beta")
			if !results[0].Success || !results[1].Success || results[1].RecoveryCopy == "" {
				t.Fatal(results)
			}
			if runTestGit(t, "git", path, "rev-parse", "HEAD") != target || runTestGit(t, "git", path, "status", "--porcelain") != "" {
				t.Fatal("not converged")
			}
			if runTestGit(t, "git", path, "remote", "get-url", "origin") != "https://example.invalid/private.git" {
				t.Fatal("sync rewrote configured upstream")
			}
			scratch := filepath.Join(t.TempDir(), "restored")
			if err := copyRepository(results[1].RecoveryCopy, scratch); err != nil {
				t.Fatal(err)
			}
			checkOriginal(scratch)
			for _, item := range run("acme/alpha", "acme/beta") {
				if !item.Success || item.RecoveryCopy != "" {
					t.Fatal(item)
				}
			}
			copies, err := os.ReadDir(recovery)
			if err != nil || len(copies) != 1 {
				t.Fatal(copies, err)
			}
		})
	}
}

func TestExplicitSyncRecoversHiddenEditAtReadyCommit(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	_, store, _ := activationStore(t)
	ctx := t.Context()
	id := "acme/beta"
	if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: "beta", Source: url + "/acme/beta.git", Commit: commits["beta"]}); err != nil {
		t.Fatal(err)
	}
	sync := func(ctx context.Context) PackageSynchronization {
		t.Helper()
		rows, err := store.SynchronizePackages(ctx, []string{id})
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		return rows[0]
	}
	if row := sync(ctx); !row.Success {
		t.Fatal(row)
	}
	dir := store.packagePath(id)
	manifest := filepath.Join(dir, "package.toml")
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			edit := string(original) + "\n# unpublished\n"
			writeFile(t, manifest, edit)
			if row := sync(ctx); row.Success || row.RecoveryCopy != "" {
				t.Fatal("ordinary install replaced dirty work", row)
			}
			runTestGit(t, "git", dir, "update-index", flag, "package.toml")
			if runTestGit(t, "git", dir, "status", "--porcelain") != "" {
				t.Fatal("control edit not hidden")
			}
			row := sync(WithCheckoutRecovery(ctx))
			if !row.Success || row.RecoveryCopy == "" || row.Commit != commits["beta"] {
				t.Fatal(row)
			}
			scratch := filepath.Join(t.TempDir(), "restored")
			if err := copyRepository(row.RecoveryCopy, scratch); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{manifest: string(original), filepath.Join(scratch, "package.toml"): edit} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("wrong checkout bytes at %s: %v", path, err)
				}
			}
			if row := sync(WithCheckoutRecovery(ctx)); !row.Success || row.RecoveryCopy != "" {
				t.Fatal(row)
			}
		})
	}
}

func TestOrdinarySynchronizationDoesNotReplayFailedSet(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	root, store, db := activationStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.SetPackageIndex(t.Context(), PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git", Commit: commits[name]}); err != nil {
			t.Fatal(err)
		}
	}
	events := []string{}
	runner := &activationJobRecorder{events: &events, calls: map[string]int{}, failures: map[string]int{"acme/beta/post-activate:acme/beta": 1}}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	store.SetSchemaDeployment(coordinator)
	ids := []string{"acme/alpha", "acme/beta"}
	results, err := store.SynchronizePackages(t.Context(), ids)
	if err != nil || len(results) != 2 || results[0].Success || results[1].Success {
		t.Fatalf("premature acknowledgement: %+v %v", results, err)
	}
	assertActivationState(t, db, "code_switched")
	assertHookAttempts(t, db, "acme/alpha", "post-activate", "succeeded", 1)
	assertHookAttempts(t, db, "acme/beta", "post-activate", "failed", 1)
	for _, id := range ids {
		entry, _, err := store.index.Get(t.Context(), id)
		if err != nil || entry.ActiveCommit != "" {
			t.Fatalf("premature publication: %+v %v", entry, err)
		}
	}
	retained, err := filepath.Glob(filepath.Join(root, "packages/acme/.*-sync-*"))
	if err != nil {
		t.Fatal(err)
	}
	results, err = store.SynchronizePackages(t.Context(), ids)
	if err != nil || results[0].Success || results[1].Success {
		t.Fatalf("pending set accepted: %+v %v", results, err)
	}
	assertHookAttempts(t, db, "acme/alpha", "post-activate", "succeeded", 1)
	assertHookAttempts(t, db, "acme/beta", "post-activate", "failed", 1)
	after, err := filepath.Glob(filepath.Join(root, "packages/acme/.*-sync-*"))
	if err != nil || strings.Join(after, "\n") != strings.Join(retained, "\n") {
		t.Fatalf("rejected retry leaked staging: %v -> %v, %v", retained, after, err)
	}
}

func TestOrdinarySynchronizationOutcomeFailureAbortsPreparation(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	root, store, _ := activationStore(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.SetPackageIndex(t.Context(), PackageIndex{Author: "acme", Repository: name, Source: url + "/acme/" + name + ".git", Commit: commits[name]}); err != nil {
			t.Fatal(err)
		}
	}
	hook := &outcomeFailure{}
	store.SetSchemaDeployment(hook)
	results, err := store.SynchronizePackages(t.Context(), []string{"acme/alpha", "acme/beta"})
	if err != nil || results[0].Success || results[1].Success || !strings.Contains(results[0].Error, "outcome read failed") || !strings.Contains(results[0].Error, "cleanup failed") {
		t.Fatalf("lost outcome/cleanup failure: %+v %v", results, err)
	}
	if len(hook.prepared) != 2 || len(hook.completed) != 1 || hook.completed[0] {
		t.Fatalf("preparation not aborted: %+v", hook)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(root, "packages/acme", name)); !os.IsNotExist(err) {
			t.Fatalf("source switched after outcome failure: %v", err)
		}
	}
}
