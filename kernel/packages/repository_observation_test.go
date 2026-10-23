package packages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryObservationDistinguishesUninstalledSource(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	ctx := context.Background()
	path := filepath.Join(root, "packages", "acme", "app")
	check := func(status string) {
		t.Helper()
		repository, err := store.InspectPackageRepository(ctx, "acme/app")
		if err != nil || repository.Status != status || repository.Head != "" || repository.ActivationReady {
			t.Fatalf("want %s, got %#v, %v", status, repository, err)
		}
	}
	check("absent")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	check("empty")
	writeFile(t, filepath.Join(path, "package.toml"), "schema = 1\n")
	check("not-initialized")
	writeFile(t, filepath.Join(path, ".git"), "corrupt")
	check("invalid-git-metadata")
	if err := os.Remove(filepath.Join(path, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	check("missing-initial-commit")

	unlock, err := store.lockPackage(ctx, "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InspectPackageRepository(ctx, "acme/app"); err == nil {
		t.Error("observation must reject active publication")
	}
	unlock()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.InspectPackageRepository(cancelled, "acme/app"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InspectPackageRepository(ctx, "acme/app"); err == nil {
		t.Error("unsafe source must not appear absent")
	}
}
