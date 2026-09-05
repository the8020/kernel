package packages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"the8020/kernel/deployment"
)

func writeIndexedHandlers(t *testing.T, root, id string) {
	t.Helper()
	writeHandlerProgram(t, root, "run")
	writeFile(t, filepath.Join(root, "events", "arbitrary name.toml"), fmt.Sprintf("event = %q\ndescription = %q\nprogram = %q\n", "minute", "Run", id+"/run"))
	writeFile(t, filepath.Join(root, "hooks", "unrelated name.toml"), fmt.Sprintf("hook = %q\ndescription = %q\nprogram = %q\n", "post-activate", "Setup", id+"/run"))
}

func TestScopedHandlerReindexReplacesBothKindsWithoutReadingOtherDeclarations(t *testing.T) {
	root, store, _ := activationStore(t)
	ctx := context.Background()
	for _, id := range []string{"acme/one", "acme/two"} {
		writeIndexedHandlers(t, filepath.Join(root, "packages", id), id)
		putActivePackage(t, store, id, "first")
	}
	if report, err := store.ReindexHandlers(ctx); err != nil || report != (HandlerReport{Events: 2, Hooks: 2}) {
		t.Fatalf("initial report=%#v error=%v", report, err)
	}
	// Unselected source is deliberately invalid: a scoped refresh must retain
	// its cached declarations without discovering that filesystem change.
	writeFile(t, filepath.Join(root, "packages/acme/two/events/arbitrary name.toml"), "invalid = [")
	one := filepath.Join(root, "packages/acme/one")
	writeFile(t, filepath.Join(one, "events/arbitrary name.toml"), "event = \"changed\"\ndescription = \"Changed\"\nprogram = \"acme/one/run\"\n")
	if err := os.Remove(filepath.Join(one, "hooks/unrelated name.toml")); err != nil {
		t.Fatal(err)
	}
	if report, err := store.ReindexHandlers(ctx, "acme/one", "acme/one"); err != nil || report != (HandlerReport{Events: 2, Hooks: 1}) {
		t.Fatalf("scoped report=%#v error=%v", report, err)
	}
	if len(store.EventListeners("changed")) != 1 || len(store.EventListeners("minute")) != 1 {
		t.Fatal("trigger replacement lost the unselected package or retained a stale event")
	}
	if len(store.PackageHooks("acme/one", "post-activate")) != 0 {
		t.Fatal("removed hook remained indexed")
	}
	if len(store.PackageHooks("acme/two", "post-activate")) != 1 {
		t.Fatal("unselected hook disappeared")
	}
	before := store.EventListeners("changed")
	writeFile(t, filepath.Join(one, "events/arbitrary name.toml"), "event = \"unpublished\"\ndescription = \"Changed\"\nprogram = \"acme/one/run\"\n")
	writeFile(t, filepath.Join(one, "hooks/broken.toml"), "hook = \"post-activate\"\n")
	if _, err := store.ReindexHandlers(ctx, "acme/one"); err == nil {
		t.Fatal("invalid hook accepted")
	}
	if len(store.EventListeners("unpublished")) != 0 || !reflect.DeepEqual(before, store.EventListeners("changed")) {
		t.Fatal("failed refresh partially published events")
	}
	if _, err := store.ReindexHandlers(ctx); err == nil {
		t.Fatal("full reindex did not read invalid declarations")
	}
	if err := store.index.SetActivation(ctx, "acme/one", "failed", "", nil); err != nil {
		t.Fatal(err)
	}
	if report, err := store.ReindexHandlers(ctx, "acme/one"); err != nil || report != (HandlerReport{Events: 1, Hooks: 1}) || len(store.EventListeners("changed")) != 0 {
		t.Fatalf("inactive package removal report=%#v error=%v", report, err)
	}
}

func TestScopedTargetCommitRefreshUpdatesCachedCrossPackageReferences(t *testing.T) {
	root, store, _ := activationStore(t)
	ctx := context.Background()
	owner, target := filepath.Join(root, "packages/acme/owner"), filepath.Join(root, "packages/acme/target")
	writeHandlerProgram(t, target, "run")
	declaration := "description = \"Shared program\"\nprogram = \"acme/target/run\"\n"
	writeFile(t, filepath.Join(owner, "events/anything.toml"), "event = \"minute\"\n"+declaration)
	writeFile(t, filepath.Join(owner, "hooks/anything.toml"), "hook = \"pre-activate\"\n"+declaration)
	putActivePackage(t, store, "acme/owner", "owner")
	putActivePackage(t, store, "acme/target", "first")
	if _, err := store.ReindexHandlers(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(owner, "events/anything.toml"), "broken = [")
	putActivePackage(t, store, "acme/target", "second")
	if _, err := store.ReindexHandlers(ctx, "acme/target"); err != nil {
		t.Fatal(err)
	}
	items := store.EventListeners("minute")
	if len(items) != 1 || items[0].ProgramCommit != "second" {
		t.Fatalf("stale program reference: %#v", items)
	}
	items[0].ProgramCommit = "caller mutation"
	if store.EventListeners("minute")[0].ProgramCommit != "second" {
		t.Fatal("caller mutated the index")
	}
	missing := writeActivationPackage(t, t.TempDir(), "acme/target", false)
	if err := store.ValidateHandlers(ctx, []deployment.Candidate{{PackageID: "acme/target", Root: missing, Commit: "third"}}); err == nil {
		t.Fatal("candidate removed a program referenced by an indexed handler")
	}
}

func TestFlatDeclarationsRequireExplicitTriggerAndOrderMultipleHooks(t *testing.T) {
	for _, kind := range []string{"events", "hooks"} {
		for name, trigger := range map[string]string{
			"missing": "", "unknown": "trigger = \"minute\"\n",
			"invalid":    map[string]string{"events": "event = \"../bad\"\n", "hooks": "hook = \"minute\"\n"}[kind],
			"wrong kind": map[string]string{"events": "hook = \"pre-activate\"\n", "hooks": "event = \"minute\"\n"}[kind],
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, kind, "minute.toml"), trigger+"description = \"Run\"\nprogram = \"acme/app/run\"\n")
				if _, err := readPackageHandlers(root, "acme/app"); err == nil {
					t.Fatal("trigger was inferred from filename or invalid field accepted")
				}
			})
		}
		root := t.TempDir()
		writeFile(t, filepath.Join(root, kind, "minute/nested.toml"), "description = \"Nested\"\n")
		if _, err := readPackageHandlers(root, "acme/app"); err == nil {
			t.Fatal("nested declarations accepted")
		}
	}
	root := t.TempDir()
	writeIndexedHandlers(t, root, "acme/app")
	if err := os.Rename(filepath.Join(root, "hooks/unrelated name.toml"), filepath.Join(root, "hooks/.hidden.toml")); err != nil {
		t.Fatal(err)
	}
	if handlers, err := HookHandlers(root); err != nil || len(handlers) != 1 {
		t.Fatalf("hidden TOML filename changed registration: %#v %v", handlers, err)
	}
	writeFile(t, filepath.Join(root, "hooks/duplicate.toml"), "hook = \"post-activate\"\ndescription = \"Duplicate\"\nprogram = \"acme/app/run\"\n")
	handlers, err := HookHandlers(root)
	if err != nil || len(handlers["post-activate"]) != 2 || handlers["post-activate"][0].ID != "hooks/.hidden.toml" {
		t.Fatalf("stable declaration ordering: %#v %v", handlers, err)
	}
}

func TestIndexServiceHooksOrderAcrossPackagesAndRefreshReferencedCode(t *testing.T) {
	root, store, _ := activationStore(t)
	ctx := context.Background()
	for _, id := range []string{"acme/build", "acme/enhance"} {
		folder := filepath.Join(root, "packages", id)
		writeHandlerProgram(t, folder, "run")
		putActivePackage(t, store, id, "first")
	}
	for _, item := range []struct {
		pkg, name, program string
		order              int
	}{
		{"acme/build", "filter", "acme/build/run", 20},
		{"acme/enhance", "build", "acme/build/run", -10},
		{"acme/enhance", "enhance", "acme/enhance/run", 0},
	} {
		writeFile(t, filepath.Join(root, "packages", item.pkg, "hooks", item.name+".toml"), fmt.Sprintf("hook = \"index-services\"\ndescription = \"Build index\"\nprogram = %q\norder = %d\n", item.program, item.order))
	}
	if report, err := store.ReindexHandlers(ctx); err != nil || report.Hooks != 3 {
		t.Fatalf("report=%#v error=%v", report, err)
	}
	chain := store.Hooks("index-services")
	if len(chain) != 3 || chain[0].ID != "acme/enhance/hooks/build.toml" || chain[1].Order != 0 || chain[2].Order != 20 {
		t.Fatalf("chain=%#v", chain)
	}
	putActivePackage(t, store, "acme/build", "second")
	if _, err := store.ReindexHandlers(ctx, "acme/build"); err != nil {
		t.Fatal(err)
	}
	if refreshed := store.Hooks("index-services"); refreshed[0].Program.Commit != "second" || chain[0].Program.Commit != "first" {
		t.Fatal("targeted code refresh was stale or mutated the prior snapshot")
	}
	chain[0].ID = "caller mutation"
	if store.Hooks("index-services")[0].ID == chain[0].ID {
		t.Fatal("caller changed the hook index")
	}
}
