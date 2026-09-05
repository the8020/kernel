package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRelationExistsDistinguishesAbsenceFromFailedLookup(t *testing.T) {
	db := New(sqliteConfig(filepath.Join(t.TempDir(), "relations.db")))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, err := RelationExists(ctx, db, "missing"); err != nil || exists {
		t.Fatalf("missing relation: %t, %v", exists, err)
	}
	for _, statement := range []string{`CREATE TABLE sample (id INTEGER)`, `CREATE VIEW sample_view AS SELECT id FROM sample`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sample", "sample_view"} {
		if exists, err := RelationExists(ctx, db, name); err != nil || !exists {
			t.Fatalf("existing relation %q: %t, %v", name, exists, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RelationExists(cancelled, db, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup looked like absence: %v", err)
	}
}

type relationLookupStore struct {
	Store
	statement string
	parameter any
	err       error
}

func (*relationLookupStore) Backend() string { return BackendPostgreSQL }
func (s *relationLookupStore) QueryRowContext(_ context.Context, statement string, parameters ...any) Row {
	s.statement, s.parameter = statement, parameters[0]
	return errorRow{err: s.err}
}

func TestPostgreSQLRelationLookupUsesVisibleCatalogAndQuotedName(t *testing.T) {
	failure := errors.New("database unavailable")
	store := &relationLookupStore{err: failure}
	if _, err := RelationExists(context.Background(), store, `Mixed"Name`); !errors.Is(err, failure) {
		t.Fatalf("catalog error was hidden: %v", err)
	}
	if store.statement != `SELECT to_regclass($1) IS NOT NULL` || store.parameter != `"Mixed""Name"` {
		t.Fatalf("relation lookup SQL=%q parameter=%q", store.statement, store.parameter)
	}
}
