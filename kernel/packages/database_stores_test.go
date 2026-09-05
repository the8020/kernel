package packages

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"the8020/kernel/database"
)

type countingDatabaseStore struct {
	database.Store
	queries int
}

func (s *countingDatabaseStore) QueryContext(ctx context.Context, statement string, arguments ...any) (*sql.Rows, error) {
	s.queries++
	return s.Store.QueryContext(ctx, statement, arguments...)
}

func (s *countingDatabaseStore) QueryRowContext(ctx context.Context, statement string, arguments ...any) database.Row {
	s.queries++
	return s.Store.QueryRowContext(ctx, statement, arguments...)
}

func packageDatabase(t *testing.T) *database.Manager {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE "the8020__packages__packages" ("packageId" TEXT PRIMARY KEY, "author" TEXT NOT NULL, "repository" TEXT NOT NULL, "source" TEXT, "requestedCommit" TEXT, "requestedTag" TEXT, "secretName" TEXT, "local" INTEGER NOT NULL, "activeCommit" TEXT, "state" TEXT NOT NULL, "error" TEXT, "revision" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__packages__activations" ("activationId" TEXT PRIMARY KEY, "stage" TEXT NOT NULL, "error" TEXT, "previousPackageSetHash" TEXT NOT NULL, "candidatePackageSetHash" TEXT NOT NULL, "startedAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL, "completedAt" TEXT) STRICT`,
		`CREATE TABLE "the8020__packages__activation_packages" ("activationId" TEXT NOT NULL, "packageId" TEXT NOT NULL, "previousCommit" TEXT, "candidateCommit" TEXT NOT NULL, "firstActivation" INTEGER NOT NULL, PRIMARY KEY ("activationId", "packageId")) STRICT`,
		`CREATE TABLE "the8020__packages__hook_runs" ("activationId" TEXT NOT NULL, "packageId" TEXT NOT NULL, "hook" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" INTEGER NOT NULL, "error" TEXT, "startedAt" TEXT, "completedAt" TEXT, PRIMARY KEY ("activationId", "packageId", "hook")) STRICT`,
		`CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE INDEX "the8020__system__revisions__revision__index" ON "the8020__system__revisions" ("revision")`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDatabasePackageIndexStoresDesiredAndActiveCommit(t *testing.T) {
	db := packageDatabase(t)
	store, err := NewDatabasePackageIndexStore(db)
	if err != nil {
		t.Fatal(err)
	}
	entry := PackageIndex{Author: "acme", Repository: "orders", PackageID: "acme/orders", Source: "https://example.test/acme/orders.git", Commit: "abcdef1", Secret: "git"}
	if err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Get(context.Background(), entry.PackageID)
	if err != nil || !exists || loaded.Source != entry.Source || loaded.Commit != entry.Commit || loaded.Secret != "git" {
		t.Fatalf("loaded=%#v exists=%t err=%v", loaded, exists, err)
	}
	if err := store.SetActivation(context.Background(), entry.PackageID, "ready", "0123456789", nil); err != nil {
		t.Fatal(err)
	}
	var state, commit string
	if err := db.QueryRowContext(context.Background(), `SELECT "state", "activeCommit" FROM "the8020__packages__packages" WHERE "packageId" = $1`, entry.PackageID).Scan(&state, &commit); err != nil || state != "ready" || commit != "0123456789" {
		t.Fatalf("state=%q commit=%q err=%v", state, commit, err)
	}
}
