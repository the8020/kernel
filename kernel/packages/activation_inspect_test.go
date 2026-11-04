package packages

import (
	"context"
	"strings"
	"testing"
)

func TestInspectExactActivation(t *testing.T) {
	db := packageDatabase(t)
	ctx := context.Background()
	statements := []string{
		`INSERT INTO "the8020__packages__activations" VALUES ('act-0123456789', 'code_switched', 'hook failed', 'before', 'after', 'now', 'now', NULL)`,
		`INSERT INTO "the8020__packages__activations" VALUES ('act-abcdefghij', 'complete', NULL, 'before', 'after', 'now', 'now', 'now')`,
		`INSERT INTO "the8020__packages__activation_packages" VALUES ('act-0123456789', 'acme/a', 'old', 'new', false)`,
		`INSERT INTO "the8020__packages__activation_packages" VALUES ('act-0123456789', 'acme/b', NULL, 'new-b', true)`,
		`INSERT INTO "the8020__packages__hook_runs" VALUES ('act-0123456789', 'acme/a', 'pre-activate', 'succeeded', 1, NULL, NULL, NULL)`,
		`INSERT INTO "the8020__packages__hook_runs" VALUES ('act-0123456789', 'acme/a', 'post-activate', 'failed', 2, 'missing import', NULL, NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	owner := &ActivationCoordinator{database: db}
	result, err := owner.InspectActivation(ctx, "act-0123456789")
	if err != nil || result.Stage != "code_switched" || result.Error != "hook failed" || len(result.Candidates) != 2 {
		t.Fatalf("inspection = %+v, %v", result, err)
	}
	first := result.Candidates[0]
	if first.PackageID != "acme/a" || first.PreviousCommit != "old" || first.Commit != "new" || len(first.Hooks) != 2 || first.Hooks[0].State != "failed" || first.Hooks[0].Attempts != 2 || first.Hooks[0].Error != "missing import" {
		t.Fatalf("member = %+v", first)
	}
	if result.Candidates[1].PackageID != "acme/b" || len(result.Candidates[1].Hooks) != 0 {
		t.Fatalf("second member = %+v", result.Candidates[1])
	}
	for _, id := range []string{"not-an-activation", "act-9999999999"} {
		if _, err := owner.InspectActivation(ctx, id); err == nil {
			t.Fatalf("inspection accepted %q", id)
		}
	}
	var stage string
	if err := db.QueryRowContext(ctx, `SELECT "stage" FROM "the8020__packages__activations" WHERE "activationId" = $1`, result.ID).Scan(&stage); err != nil || stage != result.Stage {
		t.Fatalf("inspection changed activation: %s, %v", stage, err)
	}
	if _, err := (&Store{}).InspectActivation(ctx, result.ID); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing owner = %v", err)
	}
}
