package packages

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestPackageDiscoveryUsesExactlyTwoFilesystemLevels(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\ndescription = \"Example one\"\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = 2\ndescription = \"Variables\"\n")
	writeFile(t, filepath.Join(root, "packages", "core", "missing", "README.md"), "not a package")
	writeFile(t, filepath.Join(root, "packages", ".hidden", "ignored", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "nested", "too-deep", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "bad namespace", "repository", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "core", "bad repository", "package.toml"), "schema = 1\n")

	store := newTestStore(t, root)
	items, err := store.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("packages = %#v, want valid and invalid fixed-depth packages", items)
	}
	if items[0].ID != "bad namespace/repository" || items[0].Valid || len(items[0].ValidationErrors) == 0 {
		t.Fatalf("invalid package = %#v", items[0])
	}
	if items[1].ID != "core/bad repository" || items[1].Valid || len(items[1].ValidationErrors) == 0 {
		t.Fatalf("invalid repository = %#v", items[1])
	}
	if items[2].ID != "the8020/demo" || !items[2].Valid || items[2].Description != "Example one" {
		t.Fatalf("valid package = %#v", items[2])
	}
}

func TestActivatedPackageCommitRequiresCleanExactSource(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "demo")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Demo\"\n")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", packageRoot)
	runTestGit(t, gitPath, packageRoot, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, packageRoot, "config", "user.email", "packages@example.test")
	runTestGit(t, gitPath, packageRoot, "add", ".")
	runTestGit(t, gitPath, packageRoot, "commit", "-q", "-m", "initial")
	commit := runTestGit(t, gitPath, packageRoot, "rev-parse", "HEAD")
	store := newTestStore(t, root)
	index := store.index.(*memoryPackageIndexStore)
	if err := index.Put(context.Background(), PackageIndex{
		PackageID: "the8020/demo", Author: "the8020", Repository: "demo",
		State: "ready", ActiveCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	if actual, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err != nil || actual != commit {
		t.Fatalf("activated commit = %q, %v", actual, err)
	}

	writeFile(t, filepath.Join(packageRoot, "draft.ts"), "export const draft = true;\n")
	if _, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty checkout error = %v", err)
	}
	if err := os.Remove(filepath.Join(packageRoot, "draft.ts")); err != nil {
		t.Fatal(err)
	}
	entry, _, _ := index.Get(context.Background(), "the8020/demo")
	entry.ActiveCommit = strings.Repeat("0", len(commit))
	if err := index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err == nil || !strings.Contains(err.Error(), "does not match ready active commit") {
		t.Fatalf("commit mismatch error = %v", err)
	}
}

func TestPackageInspectionReadsSelectedContentsWithoutMaterializingServiceState(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "demo")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), `schema = 1
description = "Example package"
documentation_url = "https://example.test/docs"
license = "Apache-2.0"
`)
	writeFile(t, filepath.Join(packageRoot, "README.md"), "Package documentation\n")
	writeFile(t, filepath.Join(packageRoot, ".git", "config"), "internal metadata\n")
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.toml"), `schema = 2
description = "Valid service"
[lifecycle]
service_type = "session"
[access]
mode = "authenticated"
`)
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.ts"), "export default {};\n")
	writeFile(t, filepath.Join(packageRoot, "services", "broken", "service.toml"), "schema = 99\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "dashboard", "program.toml"), `schema = 1
description = "Dashboard"
uui = true
default_layout = "layouts/main.json"
discoverable = false
`)
	writeFile(t, filepath.Join(packageRoot, "programs", "dashboard", "program.ts"), "export default () => {};\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "dashboard", "layouts", "main.json"), "{}\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "broken", "program.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(packageRoot, "programs", ".hidden", "program.toml"), "schema = 1\ndescription = \"Hidden\"\n")

	store := newTestStore(t, root)
	summaries, err := store.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != "the8020/demo" {
		t.Fatalf("package summaries = %#v", summaries)
	}
	if summaries[0].Programs != nil || summaries[0].Files != nil {
		t.Fatalf("package list performed detail inspection: %#v", summaries[0])
	}

	item, err := store.InspectPackage("the8020/demo")
	if err != nil {
		t.Fatal(err)
	}
	if !item.Valid || item.Description != "Example package" || item.DocumentationURL != "https://example.test/docs" || item.License != "Apache-2.0" {
		t.Fatalf("package metadata = %#v", item)
	}
	if len(item.Programs) != 2 || item.Programs[0].ID != "the8020/demo/broken" || item.Programs[0].Valid || item.Programs[1].ID != "the8020/demo/dashboard" || !item.Programs[1].Valid || item.Programs[1].Discoverable {
		t.Fatalf("package programs = %#v", item.Programs)
	}
	if item.Programs[1].Entrypoint != "program.ts" || item.Programs[1].DefaultLayout != "layouts/main.json" || !item.Programs[1].UUI {
		t.Fatalf("valid program metadata = %#v", item.Programs[1])
	}
	filePaths := make([]string, 0, len(item.Files))
	for _, file := range item.Files {
		filePaths = append(filePaths, file.Path)
	}
	if !sort.StringsAreSorted(filePaths) || !slices.Contains(filePaths, "README.md") || slices.Contains(filePaths, ".git/config") {
		t.Fatalf("package files = %#v", item.Files)
	}
	if item.ContentsTruncated || len(item.InspectionErrors) != 0 {
		t.Fatalf("inspection state = truncated:%t errors:%#v", item.ContentsTruncated, item.InspectionErrors)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "services", "the8020", "demo", "valid", "state.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package inspection materialized service desired state: %v", err)
	}
}

func TestPackageRootSymlinkEscapeIsReportedInvalid(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "package.toml"), "schema = 1\n")
	if err := os.MkdirAll(filepath.Join(root, "packages", "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "packages", "core", "escaped")); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, root)
	items, err := store.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Valid || !strings.Contains(strings.Join(items[0].ValidationErrors, " "), "outside") {
		t.Fatalf("packages = %#v", items)
	}
}

func TestPackageInspectionRejectsManifestSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "package.toml"), "schema = 1\ndescription = \"Outside\"\n")
	writeFile(t, filepath.Join(outside, "service.toml"), "schema = 2\n")
	writeFile(t, filepath.Join(outside, "program.toml"), "schema = 1\ndescription = \"Outside\"\n")
	packageRoot := filepath.Join(root, "packages", "core", "escaped-package")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "package.toml"), filepath.Join(packageRoot, "package.toml")); err != nil {
		t.Fatal(err)
	}
	childRoot := filepath.Join(root, "packages", "core", "escaped-children")
	writeFile(t, filepath.Join(childRoot, "package.toml"), "schema = 1\ndescription = \"Safe package\"\n")
	if err := os.MkdirAll(filepath.Join(childRoot, "services", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "service.toml"), filepath.Join(childRoot, "services", "api", "service.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(childRoot, "programs", "dashboard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "program.toml"), filepath.Join(childRoot, "programs", "dashboard", "program.toml")); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, root)
	escapedPackage, err := store.InspectPackage("core/escaped-package")
	if err != nil {
		t.Fatal(err)
	}
	if escapedPackage.Valid || escapedPackage.Description != "" || !strings.Contains(strings.Join(escapedPackage.ValidationErrors, " "), "outside") {
		t.Fatalf("escaped package manifest = %#v", escapedPackage)
	}
	escapedChildren, err := store.InspectPackage("core/escaped-children")
	if err != nil {
		t.Fatal(err)
	}
	if len(escapedChildren.Programs) != 1 || escapedChildren.Programs[0].Valid || !strings.Contains(strings.Join(escapedChildren.Programs[0].ValidationErrors, " "), "outside") {
		t.Fatalf("escaped program manifest = %#v", escapedChildren.Programs)
	}
}

func TestParseIdentityRejectsHiddenTraversalAndWrongDepth(t *testing.T) {
	for _, value := range []string{"the8020/demo", ".the8020/demo", "core/../service", "the8020/demo/service/extra", "core/example 1/service", "the8020/demo/%2f"} {
		_, packageErr := ParsePackageID(value)
		_, serviceErr := ParseServiceID(value)
		if value == "the8020/demo" {
			if packageErr != nil || serviceErr == nil {
				t.Fatalf("value=%q package=%v service=%v", value, packageErr, serviceErr)
			}
		} else if packageErr == nil || serviceErr == nil {
			t.Fatalf("unsafe identity %q accepted: package=%v service=%v", value, packageErr, serviceErr)
		}
	}
}

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := New(Config{
		WorkspaceRoot: root,
		IndexStore:    newMemoryPackageIndexStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeService(t *testing.T, root, namespace, repository, service, entrypoint string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "services", service, "service.toml"), "schema = 2\nentrypoint = \""+entrypoint+"\"\n")
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "services", service, entrypoint), "export default {};\n")
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
