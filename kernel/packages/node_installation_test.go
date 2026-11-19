package packages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"the8020/kernel/execution/jobs"
)

func TestRollingNodePackageSync(t *testing.T) {
	ctx, db := t.Context(), packageDatabase(t)
	const id = "acme/orders"
	remote := t.TempDir()
	source := writeActivationPackage(t, t.TempDir(), id, true)
	writeFile(t, filepath.Join(source, "package.toml"), "schema = 1\ndescription = 'Rolling checkout'\n")
	runTestGit(t, "git", source, "init", "-q", "-b", "main")
	commit := func(value string) string {
		writeFile(t, filepath.Join(source, "version.txt"), value)
		runTestGit(t, "git", source, "add", ".")
		runTestGit(t, "git", source, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", value)
		return runTestGit(t, "git", source, "rev-parse", "HEAD")
	}
	old, next := commit("one"), commit("two")
	runTestGit(t, "git", source, "-c", "user.name=Test", "-c", "user.email=test@example.test", "tag", "-a", "reviewed", "-m", "reviewed release", old)
	bare := filepath.Join(remote, id+".git")
	runTestGit(t, "git", "", "clone", "-q", "--bare", source, bare)
	runTestGit(t, "git", bare, "update-server-info")
	srv := httptest.NewServer(http.FileServer(http.Dir(remote)))
	defer srv.Close()
	url := srv.URL + "/" + id + ".git"
	hooks := 0
	schema := []string{}
	node := func(name string, checkout bool) *Store {
		store, err := New(Config{WorkspaceRoot: t.TempDir(), NodeID: name, Database: db})
		if err != nil {
			t.Fatal(err)
		}
		if checkout {
			path := store.packagePath(id)
			runTestGit(t, "git", "", "clone", "-q", url, path)
			runTestGit(t, "git", path, "checkout", "-q", "--detach", old)
		}
		coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Packages: store, Schema: &activationSchemaRecorder{events: &schema}, Jobs: activationRunFunc(func(context.Context, string, string, jobs.Options) (jobs.Record, error) {
			hooks++
			return jobs.Record{State: "SUCCEEDED"}, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		store.SetSchemaDeployment(coordinator)
		return store
	}
	blue, green := node("blue", true), node("green", true)
	if subject, err := blue.DescribePackageCommit(ctx, id, old); err != nil || subject != "one" {
		t.Fatalf("commit subject %q: %v", subject, err)
	}
	if got, err := blue.ResolveTransportCommit(ctx, GitTransport{Source: url}, old[:10]); err != nil || got != old {
		t.Fatalf("resolve historical commit %s: %v", got, err)
	}
	if got, err := blue.ResolveTransportCommit(ctx, GitTransport{Source: url}, "tag:reviewed"); err != nil || got != old {
		t.Fatalf("resolve annotated tag: %s, %v", got, err)
	}
	if err := blue.schemaDeployment().(*ActivationCoordinator).Bootstrap(ctx, map[string]string{id: old}); err != nil {
		t.Fatal(err)
	}
	if err := green.index.Put(ctx, PackageIndex{PackageID: id, Author: "acme", Repository: "orders", Source: url, Tag: "pending"}); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, "git", green.packagePath(id), "tag", "pending", next)
	if err := green.ValidateInstalled(ctx, map[string]string{id: old}); err != nil {
		t.Fatal(err)
	}
	if entry, _, err := green.index.Get(ctx, id); err != nil || entry.Tag != "pending" || entry.Commit != "" {
		t.Fatalf("pending tag changed: %+v, %v", entry, err)
	}
	if err := green.index.Put(ctx, PackageIndex{PackageID: id, Author: "acme", Repository: "orders", Source: url, Tag: "reviewed"}); err != nil {
		t.Fatal(err)
	}
	if err := green.ValidateInstalled(ctx, map[string]string{id: old}); err != nil {
		t.Fatal(err)
	}
	if entry, _, err := green.index.Get(ctx, id); err != nil || entry.Tag != "" || entry.Commit != old {
		t.Fatalf("installed legacy tag was not pinned: %+v, %v", entry, err)
	}
	if _, err := green.SetPackageIndex(ctx, PackageIndex{Author: "acme", Repository: "orders", Source: url, Commit: next}); err != nil {
		t.Fatal(err)
	}
	hooks = 0
	schema = nil
	selection := NodeSelection{PackageID: id, Before: old, Commit: next, Source: url}
	if err := green.SyncNodePackage(ctx, selection); err != nil {
		t.Fatal(err)
	}
	for store, want := range map[*Store]string{blue: old, green: next} {
		if got, err := store.ActivatedPackageCommit(ctx, id); err != nil || got != want {
			t.Fatalf("%s serving %s, want %s: %v", store.nodeID, got, want, err)
		}
	}
	if hooks != 2 || len(schema) != 2 {
		t.Fatalf("first rollout hooks=%d schema=%v", hooks, schema)
	}
	cold := node("new-container", false)
	selection.Before = ""
	if err := cold.SyncNodePackage(ctx, selection); err != nil {
		t.Fatal(err)
	}
	selection.Before = old
	for range 2 {
		if err := blue.SyncNodePackage(ctx, selection); err != nil {
			t.Fatal(err)
		}
	}
	if hooks != 2 || len(schema) != 2 {
		t.Fatal("catch-up replayed system activation", hooks, schema)
	}
	restore := NodeSelection{PackageID: id, Before: next, Commit: old, Source: url, Restore: true}
	if err := cold.SyncNodePackage(ctx, restore); err != nil {
		t.Fatal(err)
	}
	if actual, err := cold.ActivatedPackageCommit(ctx, id); err != nil || actual != old {
		t.Fatal("node rollback", actual, err)
	}
	if shared, _, err := green.index.Get(ctx, id); err != nil || shared.Commit != next || shared.ActiveCommit != next {
		t.Fatal("node rollback changed shared selection", shared, err)
	}
	if err := cold.SyncNodePackage(ctx, NodeSelection{PackageID: id, Before: old, Restore: true}); err != nil {
		t.Fatal(err)
	}
	if err := green.DeletePackage(ctx, id); err != nil {
		t.Fatal(err)
	}
	if actual, err := blue.ActivatedPackageCommit(ctx, id); err != nil || actual != next {
		t.Fatal("retirement affected serving peer", actual, err)
	}
	if err := blue.SyncNodePackage(ctx, NodeSelection{PackageID: id, Before: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blue.packagePath(id)); !os.IsNotExist(err) {
		t.Fatal("retired checkout still present", err)
	}
}
