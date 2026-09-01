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
)

func TestStorePersistsSortedNonDisclosingSummariesAndOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "secrets.toml")
	firstTime := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	now := firstTime
	store, err := New(Config{Path: path, Now: func() time.Time { return now }})
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
	reloaded, err := New(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := reloaded.SecretValue("github-work"); err != nil || value != "replacement-token" {
		t.Fatalf("reloaded value = %q, %v", value, err)
	}
	for _, name := range []string{path, path + ".lock"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode for %s = %v, %v", name, info.Mode().Perm(), err)
		}
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestStoreRejectsInvalidNamesValuesAndDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "secrets.toml")
	store, err := New(Config{Path: path})
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
	if err := os.WriteFile(path, []byte("schema = 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Path: path}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("invalid document error = %v", err)
	}
}

func TestStoreRejectsPublicOrLinkedSecretFiles(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets", "secrets.toml")
		store, err := New(Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Set(context.Background(), "github", "token"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.List(); err == nil || !strings.Contains(err.Error(), "private regular file") {
			t.Fatalf("public file error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target.toml")
		if err := os.WriteFile(target, []byte("schema = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "secrets", "secrets.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{Path: path}); err == nil || !strings.Contains(err.Error(), "private regular file") {
			t.Fatalf("symlink error = %v", err)
		}
	})
}

func TestStoresSerializeSharedFileMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "secrets.toml")
	left, err := New(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	right, err := New(Config{Path: path})
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
