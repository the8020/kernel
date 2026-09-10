package packages

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
)

// The first helper may lose its response after durable completion, then retry
// while a different activation has prepared but has not switched its sources.
func TestActivationTransaction(t *testing.T) {
	t.Run("UnrelatedActivationPassesWaitingHook", testIndependentActivations)
	t.Run("RecoveryLeavesLiveSourceSwitch", testLiveSourceSwitch)
	t.Run("SharedSourceLocks", testSharedSourceLocks)
	t.Run("PublicationFinishesIndexes", testPublicationFinishesIndexes)
	t.Run("RecoveryVisitsIndependentAttempts", testRecoveryVisitsIndependentAttempts)
	t.Run("PublishedPackageSnapshot", testPublishedPackageSnapshot)
	t.Run("PreparationKeepsPublishedPrograms", testPreparationKeepsPublishedPrograms)
	root, store, db := activationStore(t)
	active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	putActivePackage(t, store, "acme/orders", "old")
	events := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const first, second = "act-aaaaaaaaaa", "act-bbbbbbbbbb"
	if err := coordinator.Prepare(ctx, first, []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(ctx, second, []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "second"}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	entry, _, err := store.index.Get(ctx, "acme/orders")
	if err != nil || entry.ActiveCommit != "first" || entry.State != "ready" {
		t.Fatalf("retry of first completion published unswitched second activation: active=%s state=%s err=%v", entry.ActiveCommit, entry.State, err)
	}
	if err := coordinator.Complete(ctx, first, false); err == nil {
		t.Fatal("rolled back a completed activation")
	}
	if err := coordinator.Complete(ctx, "act-cccccccccc", true); err == nil {
		t.Fatal("completed an unknown activation")
	}
	if err := coordinator.Complete(ctx, "act-cccccccccc", false); err != nil {
		t.Fatalf("cannot abort preparation that never started: %v", err)
	}
	var secondStage string
	if err := db.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, second).Scan(&secondStage); err != nil || secondStage != "pre_activated" {
		t.Fatalf("foreign completion changed the prepared activation: %s: %v", secondStage, err)
	}
	lockedDB := &orderedActivationDatabase{Manager: db}
	restored, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: lockedDB, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Complete(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	if !lockedDB.operationReleased || lockedDB.operationLocked || lockedDB.locked {
		t.Fatal("recreated completion did not acquire and release exact activation ownership")
	}
	if err := restored.Complete(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	entry, _, err = store.index.Get(ctx, "acme/orders")
	if err != nil || entry.ActiveCommit != "first" || entry.State != "ready" {
		t.Fatalf("second rollback changed first publication: %+v: %v", entry, err)
	}
	if err := restored.Prepare(ctx, first, []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "third"}}); err == nil {
		t.Fatal("reused an activation identity")
	}
	if _, err := db.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteInitialization(ctx, map[string]string{"acme/orders": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginDeployment(ctx, second, []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "second"}}); err != nil {
		t.Fatal(err)
	}
	for _, activated := range []bool{false, true} {
		if err := db.CompleteDeployment(ctx, first, activated); err == nil {
			t.Fatal("database accepted a foreign deployment completion")
		}
	}
	if err := db.UpdatePendingDeployment(ctx, first, "failed", nil); err == nil {
		t.Fatal("database accepted a foreign deployment update")
	}
	if pending, exists, err := db.PendingDeployment(ctx); err != nil || !exists || pending.ID != second || pending.Stage != "preparing" {
		t.Fatalf("foreign request changed pending schema state: %+v: %t: %v", pending, exists, err)
	}
	if err := db.CompleteDeployment(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	rollbackCalls := 0
	schema := &activationSchemaRecorder{events: &events, onRollback: func(context.Context) error {
		rollbackCalls++
		if rollbackCalls == 1 {
			return errors.New("injected schema rollback failure")
		}
		return nil
	}}
	restored, err = NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}})
	if err != nil {
		t.Fatal(err)
	}
	const rollbackID = "act-dddddddddd"
	if err := restored.Prepare(ctx, rollbackID, []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "third"}}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Complete(ctx, rollbackID, false); err == nil {
		t.Fatal("ignored schema rollback failure")
	}
	if pending, err := restored.Pending(ctx); err != nil || !pending {
		t.Fatalf("failed rollback became terminal and cannot be recovered: pending=%t err=%v", pending, err)
	}
	if err := restored.Complete(ctx, rollbackID, false); err != nil || rollbackCalls != 2 {
		t.Fatalf("retry did not finish failed rollback: calls=%d err=%v", rollbackCalls, err)
	}
	const independentA, independentB = "act-eeeeeeeeee", "act-ffffffffff"
	if _, err := db.BeginDeployment(ctx, independentA, []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "orders-new"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginDeployment(ctx, independentB, []database.DeploymentCandidate{{PackageID: "acme/invoices", CandidateCommit: "invoices-new"}}); err != nil {
		t.Fatalf("unrelated package cannot prepare while orders is pending: %v", err)
	}
	if _, err := db.BeginDeployment(ctx, "act-gggggggggg", []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "overlap"}}); err == nil {
		t.Fatal("admitted overlapping pending package deployments")
	}
	if err := db.CompleteDeployment(ctx, independentB, true); err != nil {
		t.Fatal(err)
	}
	if !db.Status().PendingDeployment {
		t.Fatal("completing invoices hid the pending orders deployment")
	}
	if err := db.CompleteDeployment(ctx, independentA, true); err != nil {
		t.Fatal(err)
	}
	state, err := db.CatalogState(ctx)
	if err != nil || len(state.PackageCommits) != 2 || state.PackageCommits["acme/orders"] != "orders-new" || state.PackageCommits["acme/invoices"] != "invoices-new" {
		t.Fatalf("completion replaced another package's publication: %+v: %v", state, err)
	}
	if db.Status().PendingDeployment {
		t.Fatal("completed deployments remain pending")
	}
	const removal, rollback = "act-hhhhhhhhhh", "act-iiiiiiiiii"
	if _, err := db.BeginDeployment(ctx, removal, []database.DeploymentCandidate{{PackageID: "acme/orders"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginDeployment(ctx, rollback, []database.DeploymentCandidate{{PackageID: "acme/invoices", CandidateCommit: "not-published"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeployment(ctx, removal, true); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDeployment(ctx, rollback, false); err != nil {
		t.Fatal(err)
	}
	state, err = db.CatalogState(ctx)
	if err != nil || len(state.PackageCommits) != 1 || state.PackageCommits["acme/invoices"] != "invoices-new" || db.Status().PendingDeployment {
		t.Fatalf("rollback lost an unrelated removal: %+v: %v", state, err)
	}
}

func testPreparationKeepsPublishedPrograms(t *testing.T) {
	root, store, db := activationStore(t)
	ctx := context.Background()
	orders := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", false)
	writeHandlerProgram(t, orders, "read")
	tools := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/tools", false)
	writeFile(t, filepath.Join(tools, "events", "minute.toml"), "event = \"minute\"\ndescription = \"Read orders\"\nprogram = \"acme/orders/read\"\n")
	for id, path := range map[string]string{"acme/orders": orders, "acme/tools": tools} {
		commit, err := store.installedCommit(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		putActivePackage(t, store, id, commit)
	}
	if _, err := store.ReindexHandlers(ctx); err != nil {
		t.Fatal(err)
	}
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", false)
	writeHandlerProgram(t, candidate, "read")
	writeFile(t, filepath.Join(candidate, "programs/read/main.ts"), "export default () => 'candidate';\n")
	events := []string{}
	owner, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Packages: store,
		Schema: &activationSchemaRecorder{events: &events}, Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Prepare(ctx, "act-available1", []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "orders-next"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolvePackage("acme/orders"); err != nil {
		t.Errorf("preparation hid the published package: %v", err)
	}
	if _, err := store.ResolveProgram(ctx, "acme/orders/read"); err != nil {
		t.Errorf("preparation hid the published program: %v", err)
	}
	if programs, err := store.ListPrograms(ctx); err != nil || len(programs) != 1 || programs[0].ID != "acme/orders/read" {
		t.Errorf("preparation changed the published program selector: %+v: %v", programs, err)
	}
	if _, err := store.ActivatedPackageCommit(ctx, "acme/orders"); err != nil {
		t.Errorf("preparation hid unchanged published source: %v", err)
	}
	if report, err := store.ReindexHandlers(ctx); err != nil || report.Events != 1 {
		t.Errorf("preparation broke another package's handler: %+v: %v", report, err)
	}
	other := writeActivationPackage(t, t.TempDir(), "acme/tools", false)
	writeFile(t, filepath.Join(other, "hooks", "read.toml"), "hook = \"pre-activate\"\ndescription = \"Read orders\"\nprogram = \"acme/orders/read\"\n")
	if err := owner.Prepare(ctx, "act-available2", []deployment.Candidate{{PackageID: "acme/tools", Root: other, Commit: "tools-next"}}); err != nil {
		t.Errorf("preparation blocked another package's activation hook: %v", err)
	} else if err := owner.Complete(ctx, "act-available2", false); err != nil {
		t.Fatal(err)
	}
	if err := owner.Complete(ctx, "act-available1", false); err != nil {
		t.Fatal(err)
	}
}

func testPublishedPackageSnapshot(t *testing.T) {
	for _, scenario := range []string{"initialization", "preparation"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, db := activationStore(t)
			ctx := context.Background()
			commit := strings.Repeat("a", 40)
			putActivePackage(t, store, "acme/orders", commit)
			if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('packages', 7, $1)`, database.EncodeTime(db, time.Now())); err != nil {
				t.Fatal(err)
			}
			follower, err := NewPackageRevisionFollower(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			if follower.revision != 7 || follower.commits["acme/orders"] != commit {
				t.Fatalf("initial follower lost published package state: revision=%d commits=%v", follower.revision, follower.commits)
			}
			if scenario == "preparation" {
				if err := store.index.SetActivation(ctx, "acme/orders", "activating", "", nil); err != nil {
					t.Fatal(err)
				}
				// An independent publication advances the scalar while Orders
				// still has the same previously published source identity.
				if _, err := db.ExecContext(ctx, `UPDATE "the8020__system__revisions" SET "revision" = 8 WHERE "domain" = 'packages'`); err != nil {
					t.Fatal(err)
				}
				update, err := follower.Poll(ctx)
				if err != nil || update.Revision != 8 || len(update.Packages) != 0 || len(update.Paths) != 0 {
					t.Fatalf("preparation was misreported as published removal: %+v: %v", update, err)
				}
				if err := follower.Acknowledge(update.Revision); err != nil {
					t.Fatal(err)
				}
				if follower.commits["acme/orders"] != commit {
					t.Fatal("preparation discarded the last published commit")
				}
			}
		})
	}
}

// A busy publisher must not prevent recovery of other abandoned attempts.
func testRecoveryVisitsIndependentAttempts(t *testing.T) {
	root, store, db := activationStore(t)
	ctx := context.Background()
	events := []string{}
	config := ActivationCoordinatorConfig{
		Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}},
	}
	owner, err := NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"act-llllllllll", "act-mmmmmmmmmm", "act-nnnnnnnnnn"}
	for i, name := range []string{"live", "abandoned-one", "abandoned-two"} {
		packageID := "acme/" + name
		path := writeActivationPackage(t, filepath.Join(root, "packages"), packageID, false)
		previous, err := store.installedCommit(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		putActivePackage(t, store, packageID, previous)
		if err := owner.Prepare(ctx, ids[i], []deployment.Candidate{{PackageID: packageID, Root: path, Commit: "unpublished"}}); err != nil {
			t.Fatal(err)
		}
	}
	release, err := LockSources(ctx, store.packagesRoot, []string{"acme/live"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := owner.Recover(ctx); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("recovery did not report the live publisher: %v", err)
	}
	for i, id := range ids {
		want := "failed"
		if i == 0 {
			want = "pre_activated"
		}
		var stage string
		if err := db.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, id).Scan(&stage); err != nil || stage != want {
			t.Fatalf("busy activation blocked independent recovery: id=%s stage=%s want=%s: %v", id, stage, want, err)
		}
	}
	release()
	owner, err = NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, err := owner.Pending(ctx); err != nil || pending {
		t.Fatalf("released owner remains unrecovered: pending=%t: %v", pending, err)
	}
}

// Publication is durable before local indexing. A failed index refresh must
// remain retryable without publishing another revision or running hooks again.
func testPublicationFinishesIndexes(t *testing.T) {
	for _, resume := range []string{"complete", "recover", "bootstrap", "record"} {
		t.Run(resume, func(t *testing.T) {
			root, store, db := activationStore(t)
			active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
			putActivePackage(t, store, "acme/orders", "old")
			ctx := context.Background()
			events := []string{}
			runner := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
			indexCalls := 0
			expectedFailure := "injected index refresh failure"
			if resume == "record" {
				expectedFailure = "injected completion record failure"
				if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_completion_record BEFORE UPDATE ON `+activationsTable+` WHEN NEW."stage" = 'complete' BEGIN SELECT RAISE(ABORT, 'injected completion record failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			config := ActivationCoordinatorConfig{
				Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: runner,
				Reindex: func(ctx context.Context, ids []string) error {
					indexCalls++
					entry, _, err := store.index.Get(ctx, "acme/orders")
					if err != nil || entry.ActiveCommit != "new" || entry.State != "ready" {
						t.Fatalf("index refresh preceded package publication: %+v: %v", entry, err)
					}
					if indexCalls == 1 && resume != "record" {
						return errors.New("injected index refresh failure")
					}
					_, err = store.ReindexHandlers(ctx, ids...)
					return err
				},
			}
			coordinator, err := NewActivationCoordinator(config)
			if err != nil {
				t.Fatal(err)
			}
			id := "act-jjjjjjjjjj"
			if resume == "bootstrap" {
				err = coordinator.Bootstrap(ctx, map[string]string{"acme/orders": "new"})
				if queryErr := db.QueryRowContext(ctx, `SELECT "activationId" FROM `+activationsTable).Scan(&id); queryErr != nil {
					t.Fatal(queryErr)
				}
			} else {
				if err := coordinator.Prepare(ctx, id, []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "new"}}); err != nil {
					t.Fatal(err)
				}
				err = coordinator.Complete(ctx, id, true)
			}
			if err == nil || !strings.Contains(err.Error(), expectedFailure) {
				t.Fatalf("index failure was not returned: %v", err)
			}
			if pending, err := coordinator.Pending(ctx); err != nil || !pending {
				t.Fatalf("failed index refresh was marked terminal: pending=%t error=%v", pending, err)
			}
			var stage, failure string
			var completed any
			if err := db.QueryRowContext(ctx, `SELECT "stage", "error", "completedAt" FROM `+activationsTable+` WHERE "activationId" = $1`, id).Scan(&stage, &failure, &completed); err != nil || stage != "published" || completed != nil || !strings.Contains(failure, expectedFailure) {
				t.Fatalf("lost unfinished publication: stage=%s failure=%s completed=%v: %v", stage, failure, completed, err)
			}
			if err := coordinator.Complete(ctx, id, false); err == nil {
				t.Fatal("accepted rollback after package publication")
			}
			if err := coordinator.Prepare(ctx, "act-kkkkkkkkkk", []deployment.Candidate{{PackageID: "acme/orders", Root: active, Commit: "overlap"}}); err == nil {
				t.Fatal("admitted overlapping activation before index completion")
			}
			var revision int64
			if err := db.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'packages'`).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			eventCount := len(events)
			if resume == "record" {
				if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_completion_record`); err != nil {
					t.Fatal(err)
				}
			}
			coordinator, err = NewActivationCoordinator(config)
			if err != nil {
				t.Fatal(err)
			}
			if resume == "complete" || resume == "record" {
				err = coordinator.Complete(ctx, id, true)
			} else {
				err = coordinator.Recover(ctx)
			}
			if err != nil || indexCalls != 2 || len(events) != eventCount {
				t.Fatalf("recreated owner did not resume only indexing: calls=%d events=%v: %v", indexCalls, events, err)
			}
			var finalRevision int64
			if err := db.QueryRowContext(ctx, `SELECT "revision" FROM "the8020__system__revisions" WHERE "domain" = 'packages'`).Scan(&finalRevision); err != nil || finalRevision != revision {
				t.Fatalf("index retry published another revision: %d -> %d: %v", revision, finalRevision, err)
			}
			if pending, err := coordinator.Pending(ctx); err != nil || pending {
				t.Fatalf("successful index refresh remains pending: %t: %v", pending, err)
			}
			assertActivationState(t, db, "complete")
			if len(store.PackageHooks("acme/orders", "post-activate")) != 1 {
				t.Fatal("retry did not publish the native handler index")
			}
			if err := coordinator.Complete(ctx, id, true); err != nil || indexCalls != 2 {
				t.Fatalf("completed retry repeated indexing: calls=%d: %v", indexCalls, err)
			}
		})
	}
}

// A fresh process proves that native file ownership, not a Go registry, excludes
// another node process. This helper only runs with the parent's disposable root.
func TestWorkflowAnalysisSourceLockProcess(t *testing.T) {
	root := os.Getenv("THE8020_SOURCE_LOCK_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	id := os.Getenv("THE8020_SOURCE_LOCK_TEST_PACKAGE")
	release, err := LockSources(context.Background(), root, []string{id})
	if os.Getenv("THE8020_SOURCE_LOCK_TEST_BUSY") == "true" {
		if err == nil {
			release()
			t.Fatal("independent process acquired an owned package")
		}
		if !strings.Contains(err.Error(), "busy") {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if os.Getenv("THE8020_SOURCE_LOCK_TEST_HOLD") == "true" {
		fmt.Fprintln(os.Stdout, "locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

func testSharedSourceLocks(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(id string, busy, hold bool) *exec.Cmd {
		command := exec.CommandContext(ctx, executable, "-test.run=^TestWorkflowAnalysisSourceLockProcess$")
		command.Env = append(os.Environ(), "THE8020_SOURCE_LOCK_TEST_ROOT="+root,
			"THE8020_SOURCE_LOCK_TEST_PACKAGE="+id,
			fmt.Sprintf("THE8020_SOURCE_LOCK_TEST_BUSY=%t", busy), fmt.Sprintf("THE8020_SOURCE_LOCK_TEST_HOLD=%t", hold))
		return command
	}
	check := func(id string, busy bool) {
		t.Helper()
		if output, err := child(id, busy, false).CombinedOutput(); err != nil {
			t.Fatalf("independent source owner %s busy=%t: %v: %s", id, busy, err, output)
		}
	}
	validate, stopRead, err := ObserveSources(ctx, root, []string{"acme/orders"})
	if err != nil {
		t.Fatal(err)
	}
	defer stopRead()
	if err := validate(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("source read created lock metadata: %v: %v", entries, err)
	}
	release, err := LockSources(ctx, root, []string{"acme/orders", "acme/orders"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := validate(); err == nil {
		t.Fatal("source read missed the first concurrent publication")
	}
	stopRead()
	// Replacing/removing a package directory cannot replace its lock inode.
	lockPath := filepath.Join(root, ".meta/activation-locks/acme%2Forders")
	if info, err := os.Stat(lockPath); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("expected empty package lock under .meta: %v: %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".activation-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy lock directory exists: %v", err)
	}
	path := filepath.Join(root, "acme/orders")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	check("acme/orders", true)
	check("acme/invoices", false)
	if partialRelease, err := LockSources(ctx, root, []string{"acme/invoices", "acme/orders"}); err == nil {
		partialRelease()
		t.Fatal("overlapping batch acquired source ownership")
	}
	check("acme/invoices", false) // Partial acquisition released its earlier lock.
	// A forked child briefly shares the locked descriptor until exec closes it.
	// Releasing ownership must not wait for that child to finish starting.
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	inherited := -1
	for _, fd := range fds {
		if target, _ := os.Readlink("/proc/self/fd/" + fd.Name()); target == lockPath {
			number, _ := strconv.Atoi(fd.Name())
			inherited, err = unix.Dup(number)
			break
		}
	}
	if err != nil || inherited < 0 {
		t.Fatalf("duplicate source lock descriptor: %v", err)
	}
	defer unix.Close(inherited)
	release()
	validate, stopRead, err = ObserveSources(ctx, root, []string{"acme/orders"})
	if err != nil {
		t.Fatal(err)
	}
	defer stopRead()
	if err := validate(); err != nil {
		t.Fatal(err)
	}
	check("acme/orders", true)
	stopRead()
	command := child("acme/orders", false, true)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reap := sync.OnceFunc(func() { _ = command.Process.Kill(); _ = command.Wait() })
	defer reap()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire source ownership: %q: %v", line, err)
	}
	release() // A late release from the prior owner must be harmless.
	check("acme/orders", true)
	reap()
	check("acme/orders", false) // Process death releases the native lock.
}

type pausedSourceHook struct {
	deployment.SchemaHook
	prepared chan string
	resume   <-chan struct{}
}

func (h *pausedSourceHook) Prepare(ctx context.Context, id string, candidates []deployment.Candidate) error {
	if err := h.SchemaHook.Prepare(ctx, id, candidates); err != nil {
		return err
	}
	if candidates[0].PackageID == "acme/orders" {
		h.prepared <- id
		select {
		case <-h.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func testLiveSourceSwitch(t *testing.T) {
	_, store, db := activationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	commits := map[string]string{}
	for _, name := range []string{"orders", "invoices"} {
		created, err := store.CreateLocalPackage(ctx, "acme", name, name)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(created.Repository, "label.txt"), "new label\n")
		runTestGit(t, "git", created.Repository, "add", ".")
		runTestGit(t, "git", created.Repository, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "New label")
		commits["acme/"+name] = runTestGit(t, "git", created.Repository, "rev-parse", "HEAD")
		runTestGit(t, "git", created.Repository, "reset", "--hard", created.Commit)
		putActivePackage(t, store, "acme/"+name, created.Commit)
	}
	events := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store,
		Jobs: activationRunFunc(func(context.Context, string, string, jobs.Options) (jobs.Record, error) { return jobs.Record{}, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, resume := make(chan string, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	store.SetSchemaDeployment(&pausedSourceHook{SchemaHook: coordinator, prepared: prepared, resume: resume})
	firstDone := make(chan error, 1)
	go func() {
		_, err := store.CheckoutPackageRepository(ctx, "acme/orders", "", commits["acme/orders"])
		firstDone <- err
	}()
	var id string
	select {
	case id = <-prepared:
	case err := <-firstDone:
		t.Fatalf("checkout did not prepare: %v", err)
	case <-ctx.Done():
		unblock()
		<-firstDone
		t.Fatal("checkout did not reach the source-switch boundary")
	}
	err = coordinator.Recover(ctx)
	var stage string
	queryErr := db.QueryRowContext(ctx, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, id).Scan(&stage)
	if err == nil || !strings.Contains(err.Error(), "busy") || queryErr != nil || stage != "pre_activated" {
		unblock()
		checkoutErr := <-firstDone
		t.Fatalf("recovery took over a live source switch: stage=%s recovery=%v query=%v checkout=%v", stage, err, queryErr, checkoutErr)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := store.CheckoutPackageRepository(ctx, "acme/invoices", "", commits["acme/invoices"])
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			unblock()
			<-firstDone
			t.Fatalf("unrelated source publisher failed: %v", err)
		}
	case <-time.After(time.Second):
		unblock()
		<-firstDone
		<-secondDone
		t.Fatal("unrelated checkout waited for another source publisher")
	}
	unblock()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	for packageID, commit := range commits {
		entry, _, err := store.index.Get(ctx, packageID)
		if err != nil || entry.State != "ready" || entry.ActiveCommit != commit {
			t.Errorf("source publication lost %s: %+v: %v", packageID, entry, err)
		}
		if _, err := os.Stat(filepath.Join(store.packagePath(packageID), "label.txt")); err != nil {
			t.Error(err)
		}
	}
}

func testIndependentActivations(t *testing.T) {
	root, store, db := activationStore(t)
	orders := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
	invoices := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/invoices", true)
	putActivePackage(t, store, "acme/orders", "orders-old")
	putActivePackage(t, store, "acme/invoices", "invoices-old")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	events := []string{}
	runner := activationRunFunc(func(ctx context.Context, id, _ string, _ jobs.Options) (jobs.Record, error) {
		if id == "acme/orders/pre-activate" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return jobs.Record{}, ctx.Err()
			}
		}
		return jobs.Record{}, nil
	})
	config := ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: runner}
	coordinator, err := NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	const first, second = "act-jjjjjjjjjj", "act-kkkkkkkkkk"
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinator.Prepare(ctx, first, []deployment.Candidate{{PackageID: "acme/orders", Root: orders, Commit: "orders-new"}})
	}()
	select {
	case <-started:
	case err := <-firstDone:
		t.Fatalf("first activation did not reach its hook: %v", err)
	case <-ctx.Done():
		unblock()
		<-firstDone
		t.Fatal("first activation did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		err := coordinator.Prepare(ctx, second, []deployment.Candidate{{PackageID: "acme/invoices", Root: invoices, Commit: "invoices-new"}})
		if err == nil {
			err = coordinator.Complete(ctx, second, true)
		}
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			unblock()
			<-firstDone
			t.Fatalf("unrelated activation failed while orders hook waited: %v", err)
		}
	case <-time.After(time.Second):
		unblock()
		<-firstDone
		<-secondDone
		t.Fatal("unrelated activation waited for another package's hook")
	}
	// A second coordinator must see durable package ownership, while duplicate
	// requests for the operation already executing fail promptly.
	restored, err := NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Prepare(ctx, "act-llllllllll", []deployment.Candidate{{PackageID: "acme/orders", Root: orders, Commit: "overlap"}}); err == nil {
		t.Error("second coordinator admitted an overlapping package")
	}
	if err := restored.Complete(ctx, first, false); err == nil {
		t.Error("aborted an activation whose preparation is still executing")
	}
	entry, _, err := store.index.Get(ctx, "acme/invoices")
	if err != nil || entry.ActiveCommit != "invoices-new" || entry.State != "ready" {
		t.Errorf("unrelated package did not publish: %+v: %v", entry, err)
	}
	unblock()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := restored.Complete(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	for id, commit := range map[string]string{"acme/orders": "orders-new", "acme/invoices": "invoices-new"} {
		entry, _, err := store.index.Get(ctx, id)
		if err != nil || entry.ActiveCommit != commit || entry.State != "ready" {
			t.Errorf("concurrent publication lost %s: %+v: %v", id, entry, err)
		}
	}
}
