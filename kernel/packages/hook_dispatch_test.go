package packages

import (
	"context"
	"path/filepath"
	"testing"

	"the8020/kernel/execution/jobs"
)

func TestHookDispatcherReleaseTracksChainAndOtherPackageRevisions(t *testing.T) {
	root, store, db := activationStore(t)
	ctx := context.Background()
	folder := filepath.Join(root, "packages/acme/hooks")
	writeHandlerProgram(t, folder, "run")
	writeFile(t, filepath.Join(folder, "hooks/index.toml"), "hook = \"index-services\"\ndescription = \"Index\"\nprogram = \"acme/hooks/run\"\n")
	putActivePackage(t, store, "acme/hooks", "first")
	if _, err := store.ReindexHandlers(ctx); err != nil {
		t.Fatal(err)
	}
	var releases []string
	runner := activationRunFunc(func(_ context.Context, id, entrypoint string, options jobs.Options) (jobs.Record, error) {
		if id != "acme/app/index-services" || entrypoint != HookDispatcherEntrypoint || options.OwnerID != "acme/app" || options.Reuse != nil || options.Permissions != nil || options.GroupKey != "" {
			t.Fatalf("not an ordinary dispatcher job: %s %s %#v", id, entrypoint, options)
		}
		releases = append(releases, options.ReleaseID)
		return jobs.Record{State: "SUCCEEDED", Result: options.Arguments[2]}, nil
	})
	run := func() {
		t.Helper()
		if _, err := store.RunHookChain(ctx, runner, "acme/app", "index-services", store.Hooks("index-services"), map[string]any{"package_id": "acme/app"}, map[string]any{"services": []any{}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	putActivePackage(t, store, "acme/hooks", "second")
	if _, err := store.ReindexHandlers(ctx, "acme/hooks"); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('packages', 1, CURRENT_TIMESTAMP) ON CONFLICT ("domain") DO UPDATE SET "revision" = "revision" + 1`); err != nil {
		t.Fatal(err)
	}
	run()
	if releases[0] == "" || releases[0] != releases[1] || releases[1] == releases[2] || releases[2] == releases[3] {
		t.Fatalf("release compatibility did not track code versions: %#v", releases)
	}
}
