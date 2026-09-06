package dbstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/settings"
)

func TestDurationStorageUsesItsTypedValue(t *testing.T) {
	store, _ := testStore(t)
	d, err := settings.ValidateDefinition(settings.Definition{Key: "test.age", Type: settings.TypeDuration, Storage: settings.StorageGlobal, Default: "7d", Environment: "THE8020_TEST_AGE", RestartRequired: true, Description: "Duration storage test."})
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := store.Load(context.Background(), []settings.Definition{d})
	if err != nil || values[d.Key] != settings.Duration(7*24*time.Hour) {
		t.Fatal("duration default", values, err)
	}
	if _, err := store.Set(context.Background(), d, settings.Duration(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	values, _, err = store.Load(context.Background(), []settings.Definition{d})
	if err != nil || values[d.Key] != settings.Duration(48*time.Hour) {
		t.Fatal("duration reload", values, err)
	}
}

func testStore(t *testing.T) (*Store, *database.Manager) {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "the8020__system__settings" ("key" TEXT PRIMARY KEY, "value" TEXT NOT NULL, "definitionHash" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}

func testDefinition(defaultValue string) settings.Definition {
	return settings.Definition{
		Key: "platform.name", Type: settings.TypeString, Storage: settings.StorageGlobal,
		Default: defaultValue, Environment: "THE8020_PLATFORM_NAME", RestartRequired: true, Description: "name",
	}
}

func TestMissingDefaultIsInsertedOnceAndSetAdvancesRevision(t *testing.T) {
	store, _ := testStore(t)
	values, revision, err := store.Load(context.Background(), []settings.Definition{testDefinition("First")})
	if err != nil || values["platform.name"] != "First" || revision != 0 {
		t.Fatalf("initial load=%#v revision=%d err=%v", values, revision, err)
	}
	values, revision, err = store.Load(context.Background(), []settings.Definition{testDefinition("Changed recommendation")})
	if err != nil || values["platform.name"] != "First" || revision != 0 {
		t.Fatalf("changed definition overwrote value: %#v revision=%d err=%v", values, revision, err)
	}
	revision, err = store.Set(context.Background(), testDefinition("Changed recommendation"), "Operator value")
	if err != nil || revision != 1 {
		t.Fatalf("set revision=%d err=%v", revision, err)
	}
	values, revision, err = store.Load(context.Background(), []settings.Definition{testDefinition("Third")})
	if err != nil || values["platform.name"] != "Operator value" || revision != 1 {
		t.Fatalf("stored value=%#v revision=%d err=%v", values, revision, err)
	}
}
