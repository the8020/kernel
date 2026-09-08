package packages

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type countingPackageIndex struct {
	PackageIndexStore
	lists int
}

func (s *countingPackageIndex) List(ctx context.Context) ([]PackageIndex, error) {
	s.lists++
	return s.PackageIndexStore.List(ctx)
}

func TestPackageRevisionFollowerUsesCheapNoChangePathAndTargetsChangedPackage(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	working := filepath.Join(root, "packages", "acme", "demo")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", working)
	runTestGit(t, gitPath, working, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, working, "config", "user.email", "packages@example.test")
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = \"Revision test\"\n")
	writeFile(t, filepath.Join(working, "removed.ts"), "export const removed = true;\n")
	writeFile(t, filepath.Join(working, "old name.ts"), "export const renamed = true;\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "first")
	firstCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	db := packageDatabase(t)
	store, err := New(Config{WorkspaceRoot: root, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][3]string{{"../escaped", firstCommit, firstCommit}, {"acme/demo", "--output=unexpected", firstCommit}} {
		if _, err := store.changedSourcePaths(context.Background(), value[0], value[1], value[2]); err == nil {
			t.Fatalf("accepted unsafe source comparison: %v", value)
		}
	}
	entry := PackageIndex{PackageID: "acme/demo", Author: "acme", Repository: "demo", Local: true}
	if err := store.index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.index.SetActivation(context.Background(), entry.PackageID, "ready", firstCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('packages', 1, $1)`, databaseTime(db)); err != nil {
		t.Fatal(err)
	}

	counter := &countingPackageIndex{PackageIndexStore: store.index}
	store.index = counter
	follower, err := NewPackageRevisionFollower(context.Background(), store, map[string]string{"acme/demo": firstCommit})
	if err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 || counter.lists != 0 {
		t.Fatalf("unchanged poll=%#v lists=%d err=%v", update, counter.lists, err)
	}

	if err := os.Remove(filepath.Join(working, "removed.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(working, "old name.ts"), filepath.Join(working, "new name.ts")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(working, "second.ts"), "export const second = true;\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "second")
	secondCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	if err := store.index.SetActivation(context.Background(), entry.PackageID, "ready", secondCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE "the8020__system__revisions" SET "revision" = 2`); err != nil {
		t.Fatal(err)
	}

	update, err := follower.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if update.Revision != 2 || !slices.Equal(update.Packages, []string{"acme/demo"}) {
		t.Fatalf("targeted update=%#v", update)
	}
	wantPaths := []string{"/workspace/packages/acme/demo/new name.ts", "/workspace/packages/acme/demo/old name.ts", "/workspace/packages/acme/demo/removed.ts", "/workspace/packages/acme/demo/second.ts"}
	if !slices.Equal(update.Paths, wantPaths) {
		t.Fatalf("changed paths = %v", update.Paths)
	}
	if counter.lists != 1 {
		t.Fatalf("package rows loaded %d times", counter.lists)
	}
	head := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	if head != secondCommit {
		t.Fatalf("checkout=%s want=%s", head, secondCommit)
	}
	if retry, err := follower.Poll(context.Background()); err != nil || retry.Revision != 2 {
		t.Fatalf("unacknowledged revision did not retry: %#v err=%v", retry, err)
	}
	if err := follower.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 {
		t.Fatalf("acknowledged poll=%#v err=%v", update, err)
	}
}

func databaseTime(store interface{ Backend() string }) any {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
