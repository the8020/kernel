package packages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPackageIndexRemoteInspectionSynchronizationAndVersionSelection(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	remoteRoot := t.TempDir()
	working := filepath.Join(t.TempDir(), "source")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", working)
	runTestGit(t, gitPath, working, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, working, "config", "user.email", "packages@example.test")
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = \"Remote package\"\n")
	writeFile(t, filepath.Join(working, "services", "old", "service.toml"), "schema = 2\ndescription = \"Old service\"\n")
	writeFile(t, filepath.Join(working, "services", "old", "service.ts"), "export default {};\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "first")
	firstCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	runTestGit(t, gitPath, working, "tag", "v1.0.0")
	bare := filepath.Join(remoteRoot, "the8020", "demo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, gitPath, "", "clone", "-q", "--bare", working, bare)
	runTestGit(t, gitPath, bare, "update-server-info")

	server := httptest.NewTLSServer(http.FileServer(http.Dir(remoteRoot)))
	defer server.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	source := server.URL + "/the8020/demo.git"
	root := t.TempDir()
	store := newTestStore(t, root)
	ctx := context.Background()
	entry, err := store.SetPackageIndex(ctx, PackageIndex{
		Author: "the8020", Repository: "demo", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Valid || entry.PackageID != "the8020/demo" || entry.Source != source {
		t.Fatalf("package index = %#v", entry)
	}
	if info, err := os.Stat(entry.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("package index mode: info=%v err=%v", info, err)
	}

	inspection, err := store.InspectPackageSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.PackageID != "the8020/demo" || inspection.DefaultBranch != "main" || !hasSourceReference(inspection.References, "tag", "v1.0.0", firstCommit) {
		t.Fatalf("source inspection = %#v", inspection)
	}

	results, err := store.SynchronizePackages(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success || !results[0].Changed || !results[0].Cloned || results[0].Commit != firstCommit || !slices.Equal(results[0].Services, []string{"the8020/demo/old"}) {
		t.Fatalf("initial synchronization = %#v", results)
	}
	if packages, err := store.ListPackages(); err != nil || len(packages) != 1 || packages[0].ID != "the8020/demo" {
		t.Fatalf("installed packages = %#v err=%v", packages, err)
	}

	if err := os.RemoveAll(filepath.Join(working, "services", "old")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(working, "services", "new", "service.toml"), "schema = 2\ndescription = \"New service\"\n")
	writeFile(t, filepath.Join(working, "services", "new", "service.ts"), "export default {};\n")
	runTestGit(t, gitPath, working, "add", "-A")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "second")
	secondCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	runTestGit(t, gitPath, working, "push", "-q", bare, "main", "--tags")
	runTestGit(t, gitPath, bare, "update-server-info")

	results, err = store.SynchronizePackages(ctx, []string{"the8020/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success || results[0].PreviousCommit != firstCommit || results[0].Commit != secondCommit || !slices.Equal(results[0].PreviousServices, []string{"the8020/demo/old"}) || !slices.Equal(results[0].Services, []string{"the8020/demo/new"}) {
		t.Fatalf("updated synchronization = %#v", results)
	}
	versions, err := store.ListPackageVersions(ctx, "the8020/demo", 20)
	if err != nil {
		t.Fatal(err)
	}
	if versions.CurrentCommit != secondCommit || len(versions.Versions) != 2 {
		t.Fatalf("package versions = %#v", versions)
	}

	if _, err := store.SetPackageIndex(ctx, PackageIndex{
		Author: "the8020", Repository: "demo", Source: source, Tag: "v1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	results, err = store.SynchronizePackages(ctx, []string{"the8020/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success || results[0].Commit != firstCommit || results[0].Requested != "tag:v1.0.0" {
		t.Fatalf("tag synchronization = %#v", results)
	}

	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "uncommitted.txt"), "preserve me\n")
	results, err = store.SynchronizePackages(ctx, []string{"the8020/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Success || !strings.Contains(results[0].Error, "uncommitted") {
		t.Fatalf("dirty synchronization = %#v", results)
	}
}

func TestLocalPackageCreationWritesIndexManifestAndInitialCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	store := newTestStore(t, root)
	created, err := store.CreateLocalPackage(
		context.Background(), "example", "tools", "Local tools",
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.Index.PackageID != "example/tools" || !created.Index.Local || !created.Package.Valid || created.Package.Description != "Local tools" || len(created.Commit) != 40 {
		t.Fatalf("created package = %#v", created)
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "example", "tools", ".git")); err != nil {
		t.Fatalf("local Git repository: %v", err)
	}
	results, err := store.SynchronizePackages(context.Background(), []string{"example/tools"})
	if err != nil || len(results) != 1 || !results[0].Success || !results[0].Local || results[0].Changed {
		t.Fatalf("local synchronization = %#v err=%v", results, err)
	}
	if _, err := store.CreateLocalPackage(context.Background(), "example", "tools", "duplicate"); err == nil {
		t.Fatal("duplicate local package was accepted")
	}
}

func TestPackageIndexValidationRejectsUnsafeSourcesAndSelectors(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	for name, entry := range map[string]PackageIndex{
		"credentials":       {Author: "the8020", Repository: "demo", Source: "https://token@example.test/the8020/demo.git"},
		"identity mismatch": {Author: "the8020", Repository: "demo", Source: "https://example.test/other/demo.git"},
		"commit and tag":    {Author: "the8020", Repository: "demo", Source: "https://example.test/the8020/demo.git", Commit: "abcdef1", Tag: "v1"},
		"unsafe tag":        {Author: "the8020", Repository: "demo", Source: "https://example.test/the8020/demo.git", Tag: "../main"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.SetPackageIndex(context.Background(), entry); err == nil {
				t.Fatalf("unsafe package index accepted: %#v", entry)
			}
		})
	}
}

func hasSourceReference(items []SourceReference, kind, name, commit string) bool {
	return slices.ContainsFunc(items, func(item SourceReference) bool {
		return item.Kind == kind && item.Name == name && item.Commit == commit
	})
}

func runTestGit(t *testing.T, gitPath, directory string, arguments ...string) string {
	t.Helper()
	if directory != "" {
		arguments = append([]string{"-C", directory}, arguments...)
	}
	command := exec.Command(gitPath, arguments...)
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Package Test",
		"GIT_AUTHOR_EMAIL=packages@example.test",
		"GIT_COMMITTER_NAME=Package Test",
		"GIT_COMMITTER_EMAIL=packages@example.test",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
