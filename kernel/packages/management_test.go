package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

type recordingSchemaHook struct {
	prepared  []deployment.Candidate
	completed []bool
	failure   error
	inspect   func([]deployment.Candidate) error
}

func (h *recordingSchemaHook) Prepare(_ context.Context, candidates []deployment.Candidate) error {
	h.prepared = append([]deployment.Candidate(nil), candidates...)
	if h.inspect != nil {
		if err := h.inspect(candidates); err != nil {
			return err
		}
	}
	return h.failure
}

func (h *recordingSchemaHook) Complete(_ context.Context, activated bool) error {
	h.completed = append(h.completed, activated)
	return nil
}

type testSecretResolver map[string]string

func (r testSecretResolver) SecretValue(name string) (string, error) {
	value, ok := r[name]
	if !ok {
		return "", os.ErrNotExist
	}
	return value, nil
}

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
	if len(results) != 1 || !results[0].Success || !results[0].Changed || !results[0].Cloned || results[0].Commit != firstCommit {
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

	hook := &recordingSchemaHook{}
	store.SetSchemaDeployment(hook)
	results, err = store.SynchronizePackages(ctx, []string{"the8020/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success || results[0].PreviousCommit != firstCommit || results[0].Commit != secondCommit {
		t.Fatalf("updated synchronization = %#v", results)
	}
	if len(hook.prepared) != 1 || hook.prepared[0].PackageID != "the8020/demo" || hook.prepared[0].Commit != secondCommit || !slices.Equal(hook.completed, []bool{true}) {
		t.Fatalf("schema hook = prepared %#v completed %#v", hook.prepared, hook.completed)
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
	hook.failure = errors.New("schema rejected")
	results, err = store.SynchronizePackages(ctx, []string{"the8020/demo"})
	if err != nil || len(results) != 1 || results[0].Success || !strings.Contains(results[0].Error, "schema rejected") {
		t.Fatalf("rejected schema synchronization = %#v, %v", results, err)
	}
	if repository, inspectErr := store.InspectPackageRepository(ctx, "the8020/demo"); inspectErr != nil || repository.Head != secondCommit {
		t.Fatalf("schema rejection changed active package: %#v, %v", repository, inspectErr)
	}
	hook.failure = nil
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

func TestRemoveEmptyPackagePlaceholder(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := removeEmptyPackagePlaceholder(empty)
	if err != nil || !removed {
		t.Fatalf("remove empty placeholder = %t, %v", removed, err)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty placeholder still exists: %v", err)
	}

	nonempty := filepath.Join(t.TempDir(), "nonempty")
	writeFile(t, filepath.Join(nonempty, "package.toml"), "schema = 1\n")
	removed, err = removeEmptyPackagePlaceholder(nonempty)
	if err != nil || removed {
		t.Fatalf("remove real package = %t, %v", removed, err)
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
		"unsafe secret":     {Author: "the8020", Repository: "demo", Source: "https://example.test/the8020/demo.git", Secret: "../token"},
		"long secret":       {Author: "the8020", Repository: "demo", Source: "https://example.test/the8020/demo.git", Secret: strings.Repeat("a", 129)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.SetPackageIndex(context.Background(), entry); err == nil {
				t.Fatalf("unsafe package index accepted: %#v", entry)
			}
		})
	}
}

func TestPackageRepositoryPullCheckoutAndPush(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	upstream := filepath.Join(t.TempDir(), "upstream")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", upstream)
	runTestGit(t, gitPath, upstream, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, upstream, "config", "user.email", "packages@example.test")
	writeFile(t, filepath.Join(upstream, "package.toml"), "schema = 1\ndescription = \"Repository operations\"\n")
	writeFile(t, filepath.Join(upstream, "services", "old", "service.toml"), "schema = 2\ndescription = \"Old\"\n")
	runTestGit(t, gitPath, upstream, "add", ".")
	runTestGit(t, gitPath, upstream, "commit", "-q", "-m", "first")
	first := runTestGit(t, gitPath, upstream, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "repository.git")
	runTestGit(t, gitPath, "", "clone", "-q", "--bare", upstream, bare)

	root := t.TempDir()
	installed := filepath.Join(root, "packages", "example", "repo")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, gitPath, "", "clone", "-q", bare, installed)
	store := newTestStore(t, root)
	if _, err := store.SetPackageIndex(context.Background(), PackageIndex{Author: "example", Repository: "repo", Local: true}); err != nil {
		t.Fatal(err)
	}

	inspection, err := store.InspectPackageRepository(context.Background(), "example/repo")
	if err != nil || inspection.Branch != "main" || inspection.Head != first || len(inspection.Branches) != 1 || len(inspection.Commits) != 1 {
		t.Fatalf("repository inspection = %#v, %v", inspection, err)
	}
	if err := os.RemoveAll(filepath.Join(upstream, "services", "old")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(upstream, "services", "new", "service.toml"), "schema = 2\ndescription = \"New\"\n")
	runTestGit(t, gitPath, upstream, "add", "-A")
	runTestGit(t, gitPath, upstream, "commit", "-q", "-m", "second")
	second := runTestGit(t, gitPath, upstream, "rev-parse", "HEAD")
	runTestGit(t, gitPath, upstream, "push", "-q", bare, "main")

	hook := &recordingSchemaHook{inspect: func(candidates []deployment.Candidate) error {
		if len(candidates) != 1 || candidates[0].Root == installed {
			return errors.New("candidate was not staged")
		}
		if head := runTestGit(t, gitPath, installed, "rev-parse", "HEAD"); head != first {
			return fmt.Errorf("active checkout moved before schema preparation: %s", head)
		}
		return nil
	}}
	store.SetSchemaDeployment(hook)
	pulled, err := store.PullPackageRepository(context.Background(), "example/repo")
	if err != nil || !pulled.Changed || pulled.Repository.Head != second {
		t.Fatalf("pull = %#v, %v", pulled, err)
	}
	if !slices.Equal(hook.completed, []bool{true}) {
		t.Fatalf("schema completion = %#v", hook.completed)
	}
	hook.inspect = nil
	hook.failure = errors.New("schema rejected")
	if _, err := store.CheckoutPackageRepository(context.Background(), "example/repo", "", first); err == nil || !strings.Contains(err.Error(), "schema rejected") {
		t.Fatalf("rejected checkout error = %v", err)
	}
	if active := runTestGit(t, gitPath, installed, "rev-parse", "HEAD"); active != second {
		t.Fatalf("schema rejection moved active checkout to %s", active)
	}
	hook.failure = nil
	detached, err := store.CheckoutPackageRepository(context.Background(), "example/repo", "", first)
	if err != nil || !detached.Changed || detached.Repository.Branch != "" || detached.Repository.Head != first {
		t.Fatalf("commit checkout = %#v, %v", detached, err)
	}
	main, err := store.CheckoutPackageRepository(context.Background(), "example/repo", "main", "")
	if err != nil || !main.Changed || main.Repository.Branch != "main" || main.Repository.Head != second {
		t.Fatalf("branch checkout = %#v, %v", main, err)
	}
	runTestGit(t, gitPath, installed, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, installed, "config", "user.email", "packages@example.test")
	writeFile(t, filepath.Join(installed, "README.md"), "local commit\n")
	runTestGit(t, gitPath, installed, "add", "README.md")
	runTestGit(t, gitPath, installed, "commit", "-q", "-m", "third")
	third := runTestGit(t, gitPath, installed, "rev-parse", "HEAD")
	pushed, err := store.PushPackageRepository(context.Background(), "example/repo")
	if err != nil || pushed.Head != third {
		t.Fatalf("push = %#v, %v", pushed, err)
	}
	if remoteHead := runTestGit(t, gitPath, bare, "rev-parse", "refs/heads/main"); remoteHead != third {
		t.Fatalf("remote head = %s, want %s", remoteHead, third)
	}
	writeFile(t, filepath.Join(installed, "dirty.txt"), "not committed\n")
	if _, err := store.PullPackageRepository(context.Background(), "example/repo"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty pull error = %v", err)
	}
}

func TestPackageRepositoryUsesSelectedSecretWithoutPersistingOrPassingPlaintext(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	repository := filepath.Join(root, "packages", "example", "repo")
	writeFile(t, filepath.Join(repository, "package.toml"), "schema = 1\ndescription = \"Authenticated\"\n")
	runTestGit(t, gitPath, repository, "init", "-q", "-b", "main")
	runTestGit(t, gitPath, repository, "add", "package.toml")
	runTestGit(t, gitPath, repository, "commit", "-q", "-m", "initial")
	runTestGit(t, gitPath, repository, "remote", "add", "origin", "https://github.com/example/repo.git")
	capture := filepath.Join(t.TempDir(), "push-environment")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in *\" push \"*) env > \"$TEST_GIT_CAPTURE\"; exit 0;; esac\nexec \"$TEST_REAL_GIT\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_GIT_CAPTURE", capture)
	t.Setenv("TEST_REAL_GIT", gitPath)
	const token = "github-plain-token"
	store, err := New(Config{
		WorkspaceRoot: root, GitPath: wrapper,
		Secrets:    testSecretResolver{"github": token},
		IndexStore: newMemoryPackageIndexStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.SetPackageIndex(context.Background(), PackageIndex{
		Author: "example", Repository: "repo", Source: "https://github.com/example/repo.git", Secret: "github",
	})
	if err != nil {
		t.Fatal(err)
	}
	if index.Secret != "github" {
		t.Fatalf("package metadata = %#v", index)
	}
	if _, err := store.PushPackageRepository(context.Background(), "example/repo"); err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	expectedHeader := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if !strings.Contains(string(environment), "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader") || !strings.Contains(string(environment), "GIT_CONFIG_VALUE_0=Authorization: Basic "+expectedHeader) || strings.Contains(string(environment), token) {
		t.Fatalf("authenticated Git environment = %s", environment)
	}
	store.secrets = testSecretResolver{}
	if _, err := store.PushPackageRepository(context.Background(), "example/repo"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing selected secret error = %v", err)
	}
	store.secrets = nil
	runTestGit(t, gitPath, repository, "remote", "set-url", "origin", "https://embedded:credential@github.com/example/repo.git")
	if inspected, err := store.InspectPackageRepository(context.Background(), "example/repo"); err != nil || strings.Contains(inspected.RemoteURL, "embedded") || strings.Contains(inspected.RemoteURL, "credential") {
		t.Fatalf("sanitized repository inspection = %#v, %v", inspected, err)
	}
	if _, err := store.PushPackageRepository(context.Background(), "example/repo"); err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("embedded remote credentials error = %v", err)
	}
}

func TestPackageSynchronizationAppliesTransientCredentialWithoutPersistingIt(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	working := filepath.Join(t.TempDir(), "source")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", working)
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = \"Private package\"\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "initial")
	remoteRoot := t.TempDir()
	bare := filepath.Join(remoteRoot, "example", "private.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, gitPath, "", "clone", "-q", "--bare", working, bare)
	runTestGit(t, gitPath, bare, "update-server-info")

	const token = "private-sync-token"
	wantedAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	files := http.FileServer(http.Dir(remoteRoot))
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != wantedAuthorization {
			writer.Header().Set("WWW-Authenticate", `Basic realm="packages"`)
			http.Error(writer, "authentication required", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(writer, request)
	}))
	defer server.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	root := t.TempDir()
	store := newTestStore(t, root)
	var logs bytes.Buffer
	store.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	if _, err := store.SetPackageIndex(context.Background(), PackageIndex{
		Author: "example", Repository: "private",
		Source: server.URL + "/example/private.git",
	}); err != nil {
		t.Fatal(err)
	}
	results, err := store.SynchronizePackagesWithCredential(context.Background(), []string{"example/private"}, token)
	if err != nil || len(results) != 1 || !results[0].Success || !results[0].Cloned {
		t.Fatalf("authenticated synchronization = %#v, %v", results, err)
	}
	index, exists, err := store.index.Get(context.Background(), "example/private")
	if err != nil || !exists || index.Secret != "" {
		t.Fatalf("transient credential changed package index = %#v, %v", index, err)
	}
	if output := logs.String(); strings.Contains(output, token) || strings.Contains(output, wantedAuthorization) {
		t.Fatal("package synchronization logged repository credentials")
	}
	encoded := strings.TrimPrefix(wantedAuthorization, "Basic ")
	if got := redactGitCredential("request failed: "+token+" Authorization: Basic "+encoded, token); strings.Contains(got, token) || strings.Contains(got, encoded) {
		t.Fatalf("credential error was not redacted: %q", got)
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
