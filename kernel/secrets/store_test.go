package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"the8020/kernel/database"
)

func testDatabase(t *testing.T, root string) *database.Manager {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS "the8020__secrets__secrets" ("name" TEXT PRIMARY KEY, "value" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestStorePersistsSortedNonDisclosingSummariesAndOverwrite(t *testing.T) {
	root := t.TempDir()
	db := testDatabase(t, root)
	firstTime := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	now := firstTime
	store, err := New(Config{Database: db, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if secrets, err := store.List(); err != nil || len(secrets) != 0 {
		t.Fatalf("empty list = %#v, %v", secrets, err)
	}
	if _, err := store.Set(context.Background(), "github-work", "first-token"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := store.Set(context.Background(), "alpha", "alpha-token"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	summary, err := store.Set(context.Background(), "github-work", "replacement-token")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Name != "github-work" || !summary.UpdatedAt.Equal(now) {
		t.Fatalf("summary = %#v", summary)
	}
	items, err := store.List()
	if err != nil || len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "github-work" {
		t.Fatalf("list = %#v, %v", items, err)
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), "token") {
		t.Fatalf("summary JSON disclosed value: %s, %v", encoded, err)
	}
	secret, err := store.Get("github-work")
	if err != nil || secret.Value != "replacement-token" || !secret.UpdatedAt.Equal(now) {
		t.Fatalf("secret = %#v, %v", secret, err)
	}
	reloaded, err := New(Config{Database: testDatabase(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := reloaded.SecretValue("github-work"); err != nil || value != "replacement-token" {
		t.Fatalf("reloaded value = %q, %v", value, err)
	}
}

func TestStoreRejectsInvalidNamesAndValues(t *testing.T) {
	store, err := New(Config{Database: testDatabase(t, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "-token", "contains space", "name/slash"} {
		if _, err := store.Set(context.Background(), name, "value"); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	if _, err := store.Set(context.Background(), "valid", ""); err == nil {
		t.Fatal("accepted empty value")
	}
	if _, err := store.Get("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestStoresSerializeSharedDatabaseMutations(t *testing.T) {
	db := testDatabase(t, t.TempDir())
	left, err := New(Config{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	right, err := New(Config{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 2)
	for name, store := range map[string]*Store{"left": left, "right": right} {
		go func(name string, store *Store) {
			<-start
			_, err := store.Set(context.Background(), name, name+"-value")
			errors <- err
		}(name, store)
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	items, err := left.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("shared list = %#v, %v", items, err)
	}
}
