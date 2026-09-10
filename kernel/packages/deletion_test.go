package packages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

func TestDeletePackagePublishesRemovalAndPreservesFailures(t *testing.T) {
	_, store, db := activationStore(t)
	ctx := context.Background()
	created, err := store.CreateLocalPackage(ctx, "acme", "example", "Deletion test")
	if err != nil {
		t.Fatal(err)
	}
	path := created.Repository
	writeHandlerProgram(t, path, "run")
	writeFile(t, filepath.Join(path, "events", "minute.toml"), "event = \"minute\"\ndescription = \"Test\"\nprogram = \"acme/example/run\"\n")
	runTestGit(t, "git", path, "add", ".")
	runTestGit(t, "git", path, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "Add program")
	commit := runTestGit(t, "git", path, "rev-parse", "HEAD")
	putActivePackage(t, store, "acme/example", commit)
	if _, err := store.ReindexHandlers(ctx); err != nil {
		t.Fatal(err)
	}
	follower, err := NewPackageRevisionFollower(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Packages: store, Schema: &activationSchemaRecorder{events: &trace},
		Jobs: &activationJobRecorder{events: &trace},
		Reindex: func(ctx context.Context, ids []string) error {
			_, err := store.ReindexHandlers(ctx, ids...)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.SetSchemaDeployment(coordinator)
	writeFile(t, filepath.Join(path, "private.txt"), "keep this work")
	if err := store.DeletePackage(ctx, "acme/example"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty deletion = %v", err)
	}
	if len(trace) != 0 {
		t.Fatalf("dirty deletion entered activation: %v", trace)
	}
	if err := os.Remove(filepath.Join(path, "private.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_retirement BEFORE UPDATE ON `+packagesTable+` WHEN NEW."state" = 'retired' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePackage(ctx, "acme/example"); err == nil {
		t.Fatal("ignored publication failure")
	}
	if _, err := os.Stat(path + ".previous"); err != nil {
		t.Fatalf("failed deletion lost recoverable source: %v", err)
	}
	if revision, err := store.index.Revision(ctx); err != nil || revision != 0 {
		t.Fatalf("partially published deletion revision=%d err=%v", revision, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_retirement`); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{path, path + ".previous"} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source remains at %s: %v", removed, err)
		}
	}
	if entries, err := store.ListPackageIndexes(); err != nil || len(entries) != 0 {
		t.Fatalf("package remains indexed: %v %v", entries, err)
	}
	if programs, err := store.ListPrograms(ctx); err != nil || len(programs) != 0 {
		t.Fatalf("program remains indexed: %v %v", programs, err)
	}
	if report, err := store.ReindexHandlers(ctx); err != nil || report.Events != 0 {
		t.Fatalf("handlers remain indexed: %v %v", report, err)
	}
	update, err := follower.Poll(ctx)
	if err != nil || !reflect.DeepEqual(update.Packages, []string{"acme/example"}) || !reflect.DeepEqual(update.Paths, []string{"/workspace/packages/acme/example/"}) {
		t.Fatalf("removed-source update=%#v err=%v", update, err)
	}
	if err := follower.Acknowledge(update.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePackage(ctx, "acme/example"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing deletion = %v", err)
	}
	// Retired identity can be created again without editing its old catalog row.
	if _, err := store.CreateLocalPackage(ctx, "acme", "example", "Recreated"); err != nil {
		t.Fatal(err)
	}
	coordinator.reindex = func(context.Context, []string) error { return errors.New("injected reindex failure") }
	if err := store.DeletePackage(ctx, "acme/example"); err == nil || !strings.Contains(err.Error(), "injected reindex failure") {
		t.Fatalf("reindex failure = %v", err)
	}
	if _, err := os.Lstat(path + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published deletion retained source after reindex failure: %v", err)
	}
}

func TestRemovalRollbackRestoresReadySourceBeforeSchemaReads(t *testing.T) {
	_, store, db := activationStore(t)
	ctx := context.Background()
	created, err := store.CreateLocalPackage(ctx, "acme", "example", "Rollback test")
	if err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	schema := &activationSchemaRecorder{events: &trace, onRollback: func(ctx context.Context) error {
		_, err := store.ActivatedPackageCommit(ctx, "acme/example")
		return err
	}}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Packages: store, Schema: schema,
		Jobs: &activationJobRecorder{events: &trace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(ctx, "act-0123456789", []deployment.Candidate{{PackageID: "acme/example", Root: created.Repository}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(ctx, "act-0123456789", false); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteUninstalledPackageAndRejectNamespaceSymlink(t *testing.T) {
	root, store, db := activationStore(t)
	ctx := context.Background()
	for _, namespace := range []string{"missing", "escape"} {
		if _, err := store.SetPackageIndex(ctx, PackageIndex{Author: namespace, Repository: "example", Local: true}); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "example", "keep"), "keep")
	if err := os.Symlink(outside, filepath.Join(root, "packages", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePackage(ctx, "escape/example"); err == nil {
		t.Fatal("followed a namespace symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "example", "keep")); err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Packages: store, Schema: &activationSchemaRecorder{events: &trace},
		Jobs: &activationJobRecorder{events: &trace},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.SetSchemaDeployment(coordinator)
	if err := store.DeletePackage(ctx, "missing/example"); err != nil {
		t.Fatal(err)
	}
}

func TestActivationRegistersNewPackageFromSource(t *testing.T) {
	root, store, db := activationStore(t)
	candidate := writeActivationPackage(t, t.TempDir(), "acme/new", false)
	ctx := context.Background()
	trace := []string{}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{
		Database: db, Packages: store, Schema: &activationSchemaRecorder{events: &trace},
		Jobs: &activationJobRecorder{events: &trace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Prepare(ctx, "act-0123456789", []deployment.Candidate{{PackageID: "acme/new", Root: candidate, Commit: "new-commit"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "packages", "acme", "new")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(candidate, path); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Complete(ctx, "act-0123456789", true); err != nil {
		t.Fatal(err)
	}
	entry, err := store.InspectPackageIndex("acme/new")
	if err != nil || !entry.Local || entry.State != "ready" || entry.ActiveCommit != "new-commit" {
		t.Fatalf("new package=%#v err=%v", entry, err)
	}
}
