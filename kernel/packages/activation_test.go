package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
)

type activationSchemaRecorder struct {
	events *[]string
}

func (r *activationSchemaRecorder) Prepare(_ context.Context, candidates []deployment.Candidate) error {
	*r.events = append(*r.events, fmt.Sprintf("schema:%d", len(candidates)))
	return nil
}

func (r *activationSchemaRecorder) Complete(_ context.Context, activated bool) error {
	*r.events = append(*r.events, fmt.Sprintf("schema-complete:%t", activated))
	return nil
}

type orderedActivationDatabase struct {
	*database.Manager
	locked          bool
	released        bool
	beginBeforeLock bool
}

func (d *orderedActivationDatabase) AcquireDeploymentLock(ctx context.Context) (context.Context, func(), error) {
	d.locked = true
	return ctx, func() {
		d.locked = false
		d.released = true
	}, nil
}

func (d *orderedActivationDatabase) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	if !d.locked {
		d.beginBeforeLock = true
	}
	return d.Manager.BeginTx(ctx, options)
}

type activationJobRecorder struct {
	events   *[]string
	failures map[string]int
	calls    map[string]int
}

type activationRunFunc func(context.Context, string, string, jobs.Options) (jobs.Record, error)

func (f activationRunFunc) Run(ctx context.Context, id, entrypoint string, options jobs.Options) (jobs.Record, error) {
	return f(ctx, id, entrypoint, options)
}

func TestHookUsesReferencedCandidateProgramAndWaitsForCompletion(t *testing.T) {
	_, store, db := activationStore(t)
	owner := writeActivationPackage(t, t.TempDir(), "acme/orders", false)
	target := writeActivationPackage(t, t.TempDir(), "other/shared", false)
	writeHandlerProgram(t, target, "prepare")
	writeHandlerProgram(t, target, "enhance")
	writeFile(t, filepath.Join(owner, "hooks", "pre-activate.toml"), "hook = \"pre-activate\"\ndescription = \"Prepare order data\"\nprogram = \"other/shared/prepare\"\n")
	writeFile(t, filepath.Join(owner, "hooks", "enhance.toml"), "hook = \"pre-activate\"\ndescription = \"Enhance order data\"\nprogram = \"other/shared/enhance\"\norder = 10\n")
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := activationRunFunc(func(ctx context.Context, id, entrypoint string, options jobs.Options) (jobs.Record, error) {
		input := options.Arguments[1].(ActivationHookContext)
		handlers := options.Arguments[0].([]HookReference)
		if id != "acme/orders/pre-activate" || entrypoint != HookDispatcherEntrypoint || options.OwnerID != "acme/orders" || options.ReleaseID == "" || input.PackageID != "acme/orders" || input.CandidateCommit != "orders-new" || input.ActivationID == "" {
			t.Errorf("resolved hook id=%s entrypoint=%s options=%#v input=%#v", id, entrypoint, options, input)
		}
		if len(handlers) != 2 || handlers[0].Entrypoint != "file:///workspace/packages/other/shared/programs/prepare/main.ts" || handlers[0].Commit != "shared-new" || handlers[1].Entrypoint != "file:///workspace/packages/other/shared/programs/enhance/main.ts" {
			t.Errorf("resolved chain = %#v", handlers)
		}
		if options.Reuse != nil || options.Permissions != nil || options.GroupKey != "" || options.User.Username != "system" {
			t.Errorf("hook overrode ordinary execution policy: %#v", options)
		}
		mounted := map[string]string{}
		for _, mount := range options.Mounts {
			if !mount.ReadOnly {
				t.Error("writable activation source")
			}
			mounted[mount.Target] = mount.Source
		}
		if mounted["/workspace/packages/acme/orders"] != owner || mounted["/workspace/packages/other/shared"] != target {
			t.Errorf("candidate mounts=%#v", mounted)
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return jobs.Record{}, ctx.Err()
		}
		return jobs.Record{}, nil
	})
	trace := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &trace}, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- coordinator.Prepare(ctx, []deployment.Candidate{{PackageID: "acme/orders", Root: owner, Commit: "orders-new"}, {PackageID: "other/shared", Root: target, Commit: "shared-new"}})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("hook did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("activation returned before hook completion: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidHookReferenceRejectsBeforeSchemaOrHooks(t *testing.T) {
	_, store, db := activationStore(t)
	root := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	writeFile(t, filepath.Join(root, "hooks", "post-activate.toml"), "hook = \"post-activate\"\ndescription = \"Unavailable program\"\nprogram = \"acme/orders/missing\"\n")
	trace := []string{}
	runner := &activationJobRecorder{events: &trace, failures: map[string]int{}, calls: map[string]int{}}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &trace}, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{{PackageID: "acme/orders", Root: root, Commit: "new"}}); err == nil {
		t.Fatal("accepted missing hook program")
	}
	if len(trace) != 0 {
		t.Fatalf("invalid declaration caused side effects: %#v", trace)
	}
}

func (r *activationJobRecorder) Run(_ context.Context, jobID, _ string, options jobs.Options) (jobs.Record, error) {
	key := jobID + ":" + options.OwnerID
	r.calls[key]++
	*r.events = append(*r.events, key)
	if options.DatabaseAccess != "" || options.ReleaseID == "" || options.Reuse != nil || options.Permissions != nil || len(options.Arguments) != 3 {
		return jobs.Record{}, errors.New("activation hook did not receive its execution contract")
	}
	if r.failures[key] > 0 {
		r.failures[key]--
		return jobs.Record{}, errors.New("injected hook failure")
	}
	return jobs.Record{State: "SUCCEEDED"}, nil
}

func TestActivationPublishesOnlyAfterBothHooks(t *testing.T) {
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	for _, phase := range []string{"pre-activate", "post-activate"} {
		if err := os.Rename(filepath.Join(candidate, "hooks", phase+".toml"), filepath.Join(candidate, "hooks", "arbitrary-"+phase+".toml")); err != nil {
			t.Fatal(err)
		}
	}
	events := []string{}
	jobs := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: jobs,
		Reindex: func(ctx context.Context, ids []string) error {
			if !reflect.DeepEqual(ids, []string{"acme/orders"}) {
				t.Fatalf("activation reindexed unrelated packages: %#v", ids)
			}
			entry, _, err := store.index.Get(ctx, ids[0])
			if err != nil || entry.State != "ready" || entry.ActiveCommit != "commit-new" {
				t.Fatalf("reindex preceded publication: %#v %v", entry, err)
			}
			events = append(events, "reindex")
			_, err = store.ReindexHandlers(ctx, ids...)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	change := deployment.Candidate{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{change}); err != nil {
		t.Fatal(err)
	}
	entry, _, err := store.index.Get(context.Background(), "acme/orders")
	if err != nil || entry.State != "activating" || entry.ActiveCommit != "commit-old" {
		t.Fatalf("candidate package was not gated before source switch: %#v err=%v", entry, err)
	}
	if _, err := store.ResolvePackage("acme/orders"); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("activating package remained available to new consumers: %v", err)
	}
	if _, err := replacePackageDirectory(active, candidate); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"schema:1", "acme/orders/pre-activate:acme/orders", "acme/orders/post-activate:acme/orders", "schema-complete:true", "reindex"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events=%#v want=%#v", events, wantEvents)
	}
	entry, _, err = store.index.Get(context.Background(), "acme/orders")
	if err != nil || entry.State != "ready" || entry.ActiveCommit != "commit-new" {
		t.Fatalf("published package=%#v err=%v", entry, err)
	}
	assertActivationState(t, db, "complete")
	assertHookAttempts(t, db, "acme/orders", "pre-activate", "succeeded", 1)
	assertHookAttempts(t, db, "acme/orders", "post-activate", "succeeded", 1)
	if handlers := store.PackageHooks("acme/orders", "post-activate"); len(handlers) != 1 || handlers[0].ProgramID != "acme/orders/post-activate" {
		t.Fatalf("published hook missing from the index: %#v", handlers)
	}
}

func TestActivationLockCoversDurableRecordThroughCompletion(t *testing.T) {
	_, store, manager := activationStore(t)
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", false)
	database := &orderedActivationDatabase{Manager: manager}
	events := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: database, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}}); err != nil {
		t.Fatal(err)
	}
	if database.beginBeforeLock || !database.locked || database.released {
		t.Fatalf("deployment lock was not retained across prepare: %#v", database)
	}
	if err := coordinator.Complete(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if database.beginBeforeLock || database.locked || !database.released {
		t.Fatalf("deployment lock lifecycle = %#v", database)
	}
}

func TestInvalidCommandCandidateLeavesPreviousPackageActive(t *testing.T) {
	root, store, database := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	writeFile(t, filepath.Join(active, "marker.txt"), "old\n")
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", false)
	validated := false
	events := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: database, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}},
		ValidateCandidates: func(_ context.Context, candidates []deployment.Candidate) error {
			validated = len(candidates) == 1 && candidates[0].Root == candidate
			return errors.New("invalid command manifest")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = coordinator.Prepare(context.Background(), []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}})
	if err == nil || !strings.Contains(err.Error(), "invalid command manifest") || !validated {
		t.Fatalf("candidate validation error=%v validated=%t", err, validated)
	}
	entry, _, readErr := store.index.Get(context.Background(), "acme/orders")
	marker, markerErr := os.ReadFile(filepath.Join(active, "marker.txt"))
	if readErr != nil || entry.State != "ready" || entry.ActiveCommit != "commit-old" || markerErr != nil || string(marker) != "old\n" || len(events) != 0 {
		t.Fatalf("active package=%#v marker=%q events=%#v read=%v marker=%v", entry, marker, events, readErr, markerErr)
	}
}

func TestFailedPreActivationKeepsPreviousPackageReady(t *testing.T) {
	root, store, db := activationStore(t)
	writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	events := []string{}
	runner := &activationJobRecorder{
		events: &events, failures: map[string]int{"acme/orders/pre-activate:acme/orders": 1}, calls: map[string]int{},
	}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = coordinator.Prepare(context.Background(), []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}})
	if err == nil || !strings.Contains(err.Error(), "injected hook failure") {
		t.Fatalf("pre-activation unexpectedly succeeded: %v", err)
	}
	entry, _, readErr := store.index.Get(context.Background(), "acme/orders")
	if readErr != nil || entry.State != "ready" || entry.ActiveCommit != "commit-old" {
		t.Fatalf("previous package not preserved: %#v err=%v", entry, readErr)
	}
	if got := events[len(events)-1]; got != "schema-complete:false" {
		t.Fatalf("schema rollback event=%q events=%#v", got, events)
	}
	assertActivationState(t, db, "failed")
	assertHookAttempts(t, db, "acme/orders", "pre-activate", "failed", 1)
}

func TestRecoveryRetriesOnlyUnfinishedPostHook(t *testing.T) {
	root, store, db := activationStore(t)
	events := []string{}
	runner := &activationJobRecorder{
		events: &events, failures: map[string]int{"acme/b/post-activate:acme/b": 1}, calls: map[string]int{},
	}
	candidates := make([]deployment.Candidate, 0, 2)
	for _, packageID := range []string{"acme/a", "acme/b"} {
		active := writeActivationPackage(t, filepath.Join(root, "packages"), packageID, false)
		putActivePackage(t, store, packageID, "commit-old")
		candidate := writeActivationPackage(t, t.TempDir(), packageID, true)
		candidates = append(candidates, deployment.Candidate{PackageID: packageID, Root: candidate, Commit: "commit-new"})
		defer func(destination string) { _ = finalizePackageDirectory(destination) }(active)
	}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(context.Background(), candidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if _, err := replacePackageDirectory(store.packagePath(candidate.PackageID), candidate.Root); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinator.Complete(context.Background(), true); err == nil {
		t.Fatal("injected post-activation failure was ignored")
	}
	for _, packageID := range []string{"acme/a", "acme/b"} {
		entry, _, err := store.index.Get(context.Background(), packageID)
		if err != nil || entry.State != "activating" || entry.ActiveCommit != "commit-old" {
			t.Fatalf("incomplete package %s=%#v err=%v", packageID, entry, err)
		}
	}
	recovered, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Prepare(context.Background(), candidates[:1]); err == nil || !strings.Contains(err.Error(), "must be recovered first") {
		t.Fatalf("new activation bypassed pending deployment: %v", err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.calls["acme/a/post-activate:acme/a"] != 1 || runner.calls["acme/b/post-activate:acme/b"] != 2 {
		t.Fatalf("post hook retries=%#v", runner.calls)
	}
	for _, packageID := range []string{"acme/a", "acme/b"} {
		entry, _, err := store.index.Get(context.Background(), packageID)
		if err != nil || entry.State != "ready" || entry.ActiveCommit != "commit-new" {
			t.Fatalf("recovered package %s=%#v err=%v", packageID, entry, err)
		}
	}
	assertActivationState(t, db, "complete")
}

func TestRecoveryKeepsFailedPostHookRetryable(t *testing.T) {
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	events := []string{}
	runner := &activationJobRecorder{
		events: &events, failures: map[string]int{"acme/orders/post-activate:acme/orders": 2}, calls: map[string]int{},
	}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	change := deployment.Candidate{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{change}); err != nil {
		t.Fatal(err)
	}
	if _, err := replacePackageDirectory(active, candidate); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(context.Background(), true); err == nil {
		t.Fatal("first post-hook failure was ignored")
	}
	firstRetry, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstRetry.Recover(context.Background()); err == nil {
		t.Fatal("second post-hook failure was ignored")
	}
	assertActivationState(t, db, "code_switched")
	secondRetry, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRetry.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.calls["acme/orders/post-activate:acme/orders"] != 3 {
		t.Fatalf("post hook attempts=%#v", runner.calls)
	}
	assertActivationState(t, db, "complete")
}

func TestRecoveryResumesMissingPreHookWhenCandidateSourceIsPresent(t *testing.T) {
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	events := []string{}
	runner := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	change := deployment.Candidate{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{change}); err != nil {
		t.Fatal(err)
	}
	if _, err := replacePackageDirectory(active, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE `+activationsTable+` SET "stage" = 'schema_synchronized'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE `+hookRunsTable+` SET "state" = 'pending', "attempts" = 0 WHERE "hook" = 'pre-activate'`); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.calls["acme/orders/pre-activate:acme/orders"] != 2 || runner.calls["acme/orders/post-activate:acme/orders"] != 1 {
		t.Fatalf("hook calls=%#v", runner.calls)
	}
	assertActivationState(t, db, "complete")
}

func TestPackagePublicationAndRevisionAreAtomic(t *testing.T) {
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	putActivePackage(t, store, "acme/orders", "commit-old")
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	events := []string{}
	runner := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	change := deployment.Candidate{PackageID: "acme/orders", Root: candidate, Commit: "commit-new"}
	if err := coordinator.Prepare(context.Background(), []deployment.Candidate{change}); err != nil {
		t.Fatal(err)
	}
	if _, err := replacePackageDirectory(active, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER reject_activation_completion BEFORE UPDATE ON `+activationsTable+` WHEN NEW."stage" = 'complete' BEGIN SELECT RAISE(ABORT, 'injected publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(context.Background(), true); err == nil {
		t.Fatal("publication failure was ignored")
	}
	entry, _, err := store.index.Get(context.Background(), "acme/orders")
	if err != nil || entry.State != "activating" || entry.ActiveCommit != "commit-old" {
		t.Fatalf("partially published package=%#v err=%v", entry, err)
	}
	if revision, err := store.index.Revision(context.Background()); err != nil || revision != 0 {
		t.Fatalf("partially published revision=%d err=%v", revision, err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TRIGGER reject_activation_completion`); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if revision, err := store.index.Revision(context.Background()); err != nil || revision != 1 {
		t.Fatalf("published revision=%d err=%v", revision, err)
	}
	assertActivationState(t, db, "complete")
}

func TestRecoveryRollsBackPartialMultiPackageSourceSwitch(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root, store, db := activationStore(t)
	candidates := make([]deployment.Candidate, 0, 2)
	oldCommits := map[string]string{}
	for _, packageID := range []string{"acme/a", "acme/b"} {
		active := writeActivationPackage(t, filepath.Join(root, "packages"), packageID, true)
		runTestGit(t, gitPath, "", "init", "-q", "-b", "main", active)
		runTestGit(t, gitPath, active, "config", "user.name", "Activation Test")
		runTestGit(t, gitPath, active, "config", "user.email", "activation@example.test")
		runTestGit(t, gitPath, active, "add", ".")
		runTestGit(t, gitPath, active, "commit", "-q", "-m", "old")
		oldCommit := runTestGit(t, gitPath, active, "rev-parse", "HEAD")
		writeFile(t, filepath.Join(active, "README.md"), packageID+" candidate\n")
		runTestGit(t, gitPath, active, "add", "README.md")
		runTestGit(t, gitPath, active, "commit", "-q", "-m", "candidate")
		candidateCommit := runTestGit(t, gitPath, active, "rev-parse", "HEAD")
		runTestGit(t, gitPath, active, "reset", "--hard", oldCommit)
		putActivePackage(t, store, packageID, oldCommit)
		oldCommits[packageID] = oldCommit
		candidates = append(candidates, deployment.Candidate{PackageID: packageID, Root: active, Commit: candidateCommit})
	}
	events := []string{}
	runner := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(context.Background(), candidates); err != nil {
		t.Fatal(err)
	}
	firstPath := store.packagePath(candidates[0].PackageID)
	runTestGit(t, gitPath, firstPath, "reset", "--hard", candidates[0].Commit)

	recovered, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		path := store.packagePath(candidate.PackageID)
		if head := runTestGit(t, gitPath, path, "rev-parse", "HEAD"); head != oldCommits[candidate.PackageID] {
			t.Fatalf("package %s remained at %s", candidate.PackageID, head)
		}
		entry, _, err := store.index.Get(context.Background(), candidate.PackageID)
		if err != nil || entry.State != "ready" || entry.ActiveCommit != oldCommits[candidate.PackageID] {
			t.Fatalf("rolled-back package %s=%#v err=%v", candidate.PackageID, entry, err)
		}
	}
	assertActivationState(t, db, "failed")
}

func TestRepositoryMutationLeavesPostFailureForDurableRecovery(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", active)
	runTestGit(t, gitPath, active, "config", "user.name", "Activation Test")
	runTestGit(t, gitPath, active, "config", "user.email", "activation@example.test")
	runTestGit(t, gitPath, active, "add", ".")
	runTestGit(t, gitPath, active, "commit", "-q", "-m", "old")
	oldCommit := runTestGit(t, gitPath, active, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(active, "README.md"), "candidate\n")
	runTestGit(t, gitPath, active, "add", "README.md")
	runTestGit(t, gitPath, active, "commit", "-q", "-m", "candidate")
	newCommit := runTestGit(t, gitPath, active, "rev-parse", "HEAD")
	runTestGit(t, gitPath, active, "reset", "--hard", oldCommit)
	putActivePackage(t, store, "acme/orders", oldCommit)

	events := []string{}
	runner := &activationJobRecorder{
		events: &events, failures: map[string]int{"acme/orders/post-activate:acme/orders": 1}, calls: map[string]int{},
	}
	schema := &activationSchemaRecorder{events: &events}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	store.SetSchemaDeployment(coordinator)
	if _, err := store.CheckoutPackageRepository(context.Background(), "acme/orders", "", newCommit); err == nil || !strings.Contains(err.Error(), "injected hook failure") {
		t.Fatalf("post failure=%v", err)
	}
	if head := runTestGit(t, gitPath, active, "rev-parse", "HEAD"); head != newCommit {
		t.Fatalf("candidate source was rolled back to %s", head)
	}
	if _, err := os.Stat(active + ".previous"); err != nil {
		t.Fatalf("previous source was not retained: %v", err)
	}
	entry, _, err := store.index.Get(context.Background(), "acme/orders")
	if err != nil || entry.State != "activating" || entry.ActiveCommit != oldCommit {
		t.Fatalf("pending package=%#v err=%v", entry, err)
	}

	recovered, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.calls["acme/orders/post-activate:acme/orders"] != 2 {
		t.Fatalf("post hook attempts=%#v", runner.calls)
	}
	entry, _, err = store.index.Get(context.Background(), "acme/orders")
	if err != nil || entry.State != "ready" || entry.ActiveCommit != newCommit {
		t.Fatalf("recovered package=%#v err=%v", entry, err)
	}
	if _, err := os.Stat(active + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous source survived completion: %v", err)
	}
}

func activationStore(t *testing.T) (string, *Store, *database.Manager) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := packageDatabase(t)
	store, err := New(Config{WorkspaceRoot: root, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	return root, store, db
}

func writeActivationPackage(t *testing.T, parent, packageID string, hooks bool) string {
	t.Helper()
	identity, err := ParsePackageID(packageID)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, identity.Namespace, identity.Repository)
	writeFile(t, filepath.Join(root, "package.toml"), "schema = 1\n")
	if hooks {
		for _, hook := range []string{"pre-activate", "post-activate"} {
			writeFile(t, filepath.Join(root, "hooks", hook+".toml"), "hook = \""+hook+"\"\ndescription = \"Activation hook\"\nprogram = \""+packageID+"/"+hook+"\"\n")
			writeHandlerProgram(t, root, hook)
		}
	}
	return root
}

func putActivePackage(t *testing.T, store *Store, packageID, commit string) {
	t.Helper()
	identity, _ := ParsePackageID(packageID)
	entry := PackageIndex{PackageID: packageID, Author: identity.Namespace, Repository: identity.Repository, Local: true}
	if err := store.index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.index.SetActivation(context.Background(), packageID, "ready", commit, nil); err != nil {
		t.Fatal(err)
	}
}

func assertActivationState(t *testing.T, db database.Store, want string) {
	t.Helper()
	var state string
	if err := db.QueryRowContext(context.Background(), `SELECT "stage" FROM `+activationsTable+` ORDER BY "startedAt" DESC LIMIT 1`).Scan(&state); err != nil || state != want {
		t.Fatalf("activation state=%q want=%q err=%v", state, want, err)
	}
}

func assertHookAttempts(t *testing.T, db database.Store, packageID, hook, wantState string, wantAttempts int) {
	t.Helper()
	var state string
	var attempts int
	if err := db.QueryRowContext(context.Background(), `SELECT "state", "attempts" FROM `+hookRunsTable+` WHERE "packageId" = $1 AND "hook" = $2`, packageID, hook).Scan(&state, &attempts); err != nil || state != wantState || attempts != wantAttempts {
		t.Fatalf("hook %s/%s state=%q attempts=%d err=%v", packageID, hook, state, attempts, err)
	}
}
