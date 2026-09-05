package packages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

func writeHandlerProgram(t *testing.T, root, name string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "programs", name, "program.toml"), "schema = 1\ndescription = \"Handler program\"\nentrypoint = \"main.ts\"\ndiscoverable = false\n")
	writeFile(t, filepath.Join(root, "programs", name, "main.ts"), "export default (...args) => args;\n")
}

func TestEventCatalogResolvesTOMLProgramsAcrossReadyPackages(t *testing.T) {
	root, store, _ := activationStore(t)
	folder := filepath.Join(root, "packages", "acme", "tools")
	shared := filepath.Join(root, "packages", "acme", "shared")
	writeHandlerProgram(t, folder, "local")
	writeHandlerProgram(t, shared, "remote")
	writeFile(t, filepath.Join(folder, "events", "first.toml"), "event = \"minute\"\ndescription = \"Local handler\"\nprogram = \"acme/tools/local\"\n")
	writeFile(t, filepath.Join(folder, "events", "second.toml"), "event = \"minute\"\ndescription = \"Shared handler\"\nprogram = \"acme/shared/remote\"\n")
	putActivePackage(t, store, "acme/tools", "tools-commit")
	putActivePackage(t, store, "acme/shared", "shared-commit")
	_, err := store.ReindexHandlers(context.Background())
	items := store.EventListeners("minute")
	if err != nil || len(items) != 2 || items[0].ProgramID != "acme/tools/local" || items[0].Description != "Local handler" || items[1].ProgramCommit != "shared-commit" {
		t.Fatalf("listeners=%#v %v", items, err)
	}
	_ = store.index.SetActivation(context.Background(), "acme/shared", "activating", "shared-commit", nil)
	if _, err = store.ReindexHandlers(context.Background()); err == nil {
		t.Fatal("accepted a reference to an unavailable program")
	}
	_ = store.index.SetActivation(context.Background(), "acme/tools", "activating", "tools-commit", nil)
	_, err = store.ReindexHandlers(context.Background())
	items = store.EventListeners("minute")
	if err != nil || len(items) != 0 {
		t.Fatalf("inactive listeners=%#v %v", items, err)
	}
}

func TestHandlerDefinitionsRejectInvalidTOMLAndIgnoreSourceFiles(t *testing.T) {
	for name, body := range map[string]string{
		"missing description": "program = \"acme/tools/run\"",
		"missing program":     "description = \"Run\"",
		"relative program":    "description = \"Run\"\nprogram = \"run\"",
		"file path":           "description = \"Run\"\nprogram = \"./programs/run/program.ts\"",
		"traversal":           "description = \"Run\"\nprogram = \"acme/../run\"",
		"unknown entrypoint":  "description = \"Run\"\nprogram = \"acme/tools/run\"\nentrypoint = \"run.ts\"",
		"invalid TOML":        "description = [",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "events", "run.toml"), "event = \"example\"\n"+body)
			writeFile(t, filepath.Join(root, "hooks", "pre-activate.toml"), "hook = \"pre-activate\"\n"+body)
			if _, err := ValidateEventListeners(root, "acme/tools"); err == nil {
				t.Fatal("accepted invalid event")
			}
			if _, err := HookHandlers(root); err == nil {
				t.Fatal("accepted invalid hook")
			}
		})
	}
	for _, kind := range []string{"events", "hooks"} {
		root := t.TempDir()
		path := filepath.Join(root, kind, "pre-activate.ts")
		writeFile(t, path, "export default () => {}")
		if kind == "hooks" {
			if handlers, err := HookHandlers(root); err != nil || len(handlers) != 0 {
				t.Fatalf("source file declared a hook: handlers=%v error=%v", handlers, err)
			}
		} else if handlers, err := ValidateEventListeners(root, "acme/tools"); err != nil || len(handlers) != 0 {
			t.Fatalf("source file declared an event: handlers=%v error=%v", handlers, err)
		}
		_ = os.Remove(path)
		outside := filepath.Join(t.TempDir(), "handler.toml")
		writeFile(t, outside, "description = \"Run\"\nprogram = \"acme/tools/run\"")
		if err := os.Symlink(outside, filepath.Join(root, kind, "pre-activate.toml")); err != nil {
			t.Fatal(err)
		}
		if kind == "hooks" {
			if _, err := HookHandlers(root); err == nil {
				t.Fatal("accepted symlink hook")
			}
		} else if _, err := ValidateEventListeners(root, "acme/tools"); err == nil {
			t.Fatal("accepted symlink event")
		}
	}
}

func TestCandidateHandlersResolveOtherCandidatesBeforePublication(t *testing.T) {
	_, store, _ := activationStore(t)
	first := writeActivationPackage(t, t.TempDir(), "acme/first", false)
	second := writeActivationPackage(t, t.TempDir(), "acme/second", false)
	writeHandlerProgram(t, second, "run")
	definition := "description = \"Shared candidate\"\nprogram = \"acme/second/run\"\n"
	writeFile(t, filepath.Join(first, "hooks", "pre-activate.toml"), "hook = \"pre-activate\"\n"+definition)
	writeFile(t, filepath.Join(first, "events", "run.toml"), "event = \"example\"\n"+definition)
	writeFile(t, filepath.Join(first, "hooks", "AGENTS.md"), "# Activation hooks\n")
	writeFile(t, filepath.Join(first, "events", "AGENTS.md"), "# Event handlers\n")
	candidates := []deployment.Candidate{{PackageID: "acme/first", Root: first, Commit: "first"}, {PackageID: "acme/second", Root: second, Commit: "second"}}
	if err := store.ValidateHandlers(context.Background(), candidates); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateHandlers(context.Background(), candidates[:1]); err == nil || !strings.Contains(err.Error(), "no ready active commit") {
		t.Fatalf("missing reference: %v", err)
	}
	if err := os.Remove(filepath.Join(second, "programs", "run", "main.ts")); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateHandlers(context.Background(), candidates); err == nil {
		t.Fatal("accepted missing program entrypoint")
	}
}
