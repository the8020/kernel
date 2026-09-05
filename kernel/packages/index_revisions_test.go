package packages

import (
	"context"
	"slices"
	"testing"
)

func TestIndexRevisionFollowerCatchesUpWithoutApplicationTables(t *testing.T) {
	ctx := context.Background()
	db := packageDatabase(t) // This fixture has no services-owned tables.
	counting := &countingDatabaseStore{Store: db}
	follower, err := NewIndexRevisionFollower(ctx, counting)
	if err != nil {
		t.Fatal(err)
	}
	counting.queries = 0
	if update, err := follower.Poll(ctx); err != nil || update.Revision != 0 || counting.queries != 1 {
		t.Fatalf("unchanged path: %#v queries=%d error=%v", update, counting.queries, err)
	}
	// Simulate several committed application edits and one missed notification.
	for _, marker := range []struct {
		domain   string
		revision int
	}{
		{"indexes", 4}, {"index:acme/changed", 4}, {"index:acme/removed", 3},
		{"index:acme/untouched", 0}, {"unrelated", 4},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ($1,$2,CURRENT_TIMESTAMP)`, marker.domain, marker.revision); err != nil {
			t.Fatal(err)
		}
	}
	update, err := follower.Poll(ctx)
	if err != nil || update.Revision != 4 || !slices.Equal(update.Packages, []string{"acme/changed", "acme/removed"}) {
		t.Fatalf("catch-up: %#v error=%v", update, err)
	}
	if err := follower.Acknowledge(3); err == nil {
		t.Fatal("acknowledged an unobserved revision")
	}
	if retry, err := follower.Poll(ctx); err != nil || !slices.Equal(retry.Packages, update.Packages) {
		t.Fatalf("retry: %#v %v", retry, err)
	}
	if err := follower.Acknowledge(4); err != nil {
		t.Fatal(err)
	}
	counting.queries = 0
	if update, err := follower.Poll(ctx); err != nil || update.Revision != 0 || counting.queries != 1 {
		t.Fatalf("acknowledged path: %#v queries=%d error=%v", update, counting.queries, err)
	}
}
