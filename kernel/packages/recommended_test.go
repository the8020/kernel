package packages

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecommendedPackagesPreserveWorkAndOnlyAdvance(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, working := t.TempDir(), t.TempDir()
	runTestGit(t, git, working, "init", "-q", "-b", "main")
	runTestGit(t, git, working, "config", "user.name", "Fixture")
	runTestGit(t, git, working, "config", "user.email", "fixture@example.test")
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = 'Recommendation fixture'\n")
	commit := func(message string) string {
		runTestGit(t, git, working, "add", ".")
		runTestGit(t, git, working, "commit", "-q", "--allow-empty", "-m", message)
		return runTestGit(t, git, working, "rev-parse", "HEAD")
	}
	first := commit("first")
	runTestGit(t, git, working, "tag", "0.9.0")
	bare := filepath.Join(remoteRoot, "acme/demo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, git, "", "clone", "-q", "--bare", working, bare)
	push := func() {
		runTestGit(t, git, working, "push", "-q", bare, "main", "--tags")
		runTestGit(t, git, bare, "update-server-info")
	}
	push()
	server := httptest.NewServer(http.FileServer(http.Dir(remoteRoot)))
	defer server.Close()
	root := t.TempDir()
	store := newTestStore(t, root)
	store.SetSchemaDeployment(&recordingSchemaHook{})
	path := filepath.Join(root, "node/kernel/scripts/bootstrap-packages.toml")
	installed := filepath.Join(root, "packages/acme/demo")
	setVersion := func(version string) {
		writeFile(t, path, fmt.Sprintf("schema = 1\n[[packages]]\nid = 'acme/demo'\nsource = '%s/acme/demo.git'\nversion = '%s'\n", server.URL, version))
	}
	apply := func(status, want string) {
		t.Helper()
		rows, err := store.InstallRecommendedPackages(t.Context())
		if err != nil || len(rows) != 1 || rows[0].Status != status {
			t.Fatalf("recommendations = %+v, %v; want %s", rows, err, status)
		}
		if got := runTestGit(t, git, installed, "rev-parse", "HEAD"); got != want {
			t.Fatalf("HEAD = %s, want %s", got, want)
		}
		if status != "installed" && status != "updated" {
			return
		}
		entry, err := store.InspectPackageIndex("acme/demo")
		if err != nil {
			t.Fatal(err)
		}
		// This fixture's schema hook has no database publication owner.
		entry.ActiveCommit, entry.State = want, "ready"
		if err := store.index.Put(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	setVersion("0.9.0")
	apply("installed", first)
	entryBefore, _ := store.InspectPackageIndex("acme/demo")
	apply("unchanged", first)
	writeFile(t, filepath.Join(working, "new.txt"), "second")
	second := commit("second")
	runTestGit(t, git, working, "tag", "0.9.1")
	push()
	apply("unchanged", first)
	setVersion("0.9.1")
	hook := &recordingSchemaHook{failure: errors.New("fixture schema rejection")}
	store.SetSchemaDeployment(hook)
	rows, err := store.InstallRecommendedPackages(t.Context())
	if err == nil || rows[0].Status != "failed" || !strings.Contains(err.Error(), "fixture schema rejection") {
		t.Fatalf("schema failure = %+v, %v", rows, err)
	}
	if got := runTestGit(t, git, installed, "rev-parse", "HEAD"); got != first {
		t.Fatal("failed activation changed source")
	}
	hook.failure = nil
	apply("updated", second)
	if len(hook.prepared) != 1 || hook.prepared[0].Commit != second {
		t.Fatal("recommendation bypassed schema activation")
	}
	setVersion("0.9.0")
	before, _ := store.InspectPackageIndex("acme/demo")
	apply("skipped", second)
	after, _ := store.InspectPackageIndex("acme/demo")
	if !reflect.DeepEqual(before, after) || after.Tag != "0.9.1" {
		t.Fatal("older recommendation changed desired state")
	}
	third := commit("untagged remote advance")
	push()
	setVersion("")
	apply("updated", third)
	writeFile(t, filepath.Join(installed, "private.txt"), "keep this work")
	before, _ = store.InspectPackageIndex("acme/demo")
	apply("skipped", third)
	raw, _ := os.ReadFile(filepath.Join(installed, "private.txt"))
	if string(raw) != "keep this work" {
		t.Fatal("uncommitted work was lost")
	}
	runTestGit(t, git, installed, "add", ".")
	runTestGit(t, git, installed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-q", "-m", "local work")
	local := runTestGit(t, git, installed, "rev-parse", "HEAD")
	// Both a locally advanced branch and a divergent branch must survive.
	apply("skipped", local)
	fourth := commit("remote diverges")
	runTestGit(t, git, working, "tag", "0.9.2")
	push()
	apply("skipped", local)
	setVersion("0.9.2")
	apply("skipped", local)
	after, _ = store.InspectPackageIndex("acme/demo")
	if after.Tag != before.Tag || after.Source != before.Source || after.Commit != before.Commit {
		t.Fatal("skipped local commits changed desired selection")
	}
	// An installed clean commit already on the remote can advance even untagged.
	runTestGit(t, git, installed, "reset", "--hard", third)
	apply("updated", fourth)
	if entryBefore.Tag != "0.9.0" {
		t.Fatal("configured initial version was not recorded")
	}
	valid, _ := os.ReadFile(path)
	writeFile(t, path, string(valid)+fmt.Sprintf("\n[[packages]]\nid = 'acme/missing'\nsource = '%s/acme/missing.git'\n[[packages]]\nid = 'acme/unreached'\nsource = '%s/acme/unreached.git'\n", server.URL, server.URL))
	rows, err = store.InstallRecommendedPackages(t.Context())
	if err == nil || len(rows) != 2 || rows[0].PackageID != "acme/demo" || rows[0].Status != "unchanged" || rows[1].Status != "failed" {
		t.Fatalf("ordered stop on failure = %+v, %v", rows, err)
	}
	if _, exists, _ := store.index.Get(t.Context(), "acme/unreached"); exists {
		t.Fatal("continued after failed recommendation")
	}
	t.Run("prepared target", func(t *testing.T) {
		writeFile(t, filepath.Join(working, "new.txt"), "maintainer 0.9.3")
		writeFile(t, filepath.Join(working, "feature/module.ts"), "export const selected = true;\n")
		target := commit("selected marker")
		runTestGit(t, git, working, "tag", "0.9.3")
		push()
		setVersion("0.9.3")
		origin := runTestGit(t, git, installed, "remote", "get-url", "origin")
		sentinel := filepath.Join(root, "packages/acme/sentinel")
		runTestGit(t, git, "", "clone", "-q", bare, sentinel)
		sentinelHead := runTestGit(t, git, sentinel, "rev-parse", "HEAD")
		sentinelOrigin := runTestGit(t, git, sentinel, "remote", "get-url", "origin")
		writeFile(t, filepath.Join(sentinel, "private.txt"), "unselected")
		writeFile(t, filepath.Join(installed, "new.txt"), "client edit")
		writeFile(t, filepath.Join(installed, "extra.txt"), "client extra")
		routes := []RecommendedSource{{PackageID: "acme/demo", Source: origin}}
		rows, err := store.InstallRecommendedPackagesFrom(t.Context(), routes)
		if err != nil || len(rows) != 1 || rows[0].Status != "skipped" || rows[0].Excluded {
			t.Fatalf("ordinary result: %+v, %v", rows, err)
		}
		if head, err := CleanRepositoryHead(t.Context(), installed); !errors.Is(err, errDirtyRepository) || head != "" {
			t.Fatalf("startup baseline unexpectedly clean: %s %v", head, err)
		}
		build := "0.10.18+" + target
		manifest, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		update := filepath.Join(root, "node/kernel/update")
		writeFile(t, filepath.Join(update, "state.json"), `{"phase":"installed","target":"0.10.18"}`)
		receipt, err := json.Marshal(map[string]any{"target": "0.10.18", "commit": target, "manifest": fmt.Sprintf("%x", sha256.Sum256(manifest)), "routes": routes})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(update, "prerequisites.json"), string(receipt))
		if err := store.RestorePreparedBaseline(t.Context(), "wrong build"); err != nil {
			t.Fatal(err)
		}
		read := func(path string) string {
			t.Helper()
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		if read(filepath.Join(installed, "new.txt")) != "client edit" {
			t.Fatal("wrong build replaced bytes")
		}
		t.Logf("before: selected=acme/demo old=%s target=%s file=%q unselected=%s", fourth, target, read(filepath.Join(installed, "new.txt")), sentinelHead)
		if err := store.RestorePreparedBaseline(t.Context(), build); err != nil {
			t.Fatal(err)
		}
		if head, err := CleanRepositoryHead(t.Context(), installed); err != nil || head != fourth || read(filepath.Join(installed, "new.txt")) != "second" {
			t.Fatalf("baseline: %s %v", head, err)
		}
		writeFile(t, filepath.Join(sentinel, "new.txt"), "unrelated edit")
		runTestGit(t, git, installed, "config", "core.worktree", sentinel)
		if _, err := store.InstallPreparedRecommendationsFrom(t.Context(), routes, build); err == nil || !strings.Contains(err.Error(), "self-contained working tree") {
			t.Fatalf("redirected checkout: %v", err)
		}
		if read(filepath.Join(sentinel, "new.txt")) != "unrelated edit" || read(filepath.Join(installed, "new.txt")) != "second" || runTestGit(t, git, installed, "rev-parse", "HEAD") != fourth {
			t.Fatal("rejected update changed a checkout")
		}
		runTestGit(t, git, installed, "config", "core.worktree", installed)
		if _, err := store.InstallPreparedRecommendationsFrom(t.Context(), routes, build); err == nil || !strings.Contains(err.Error(), "self-contained working tree") || read(filepath.Join(installed, "new.txt")) != "second" {
			t.Fatalf("staging copy retained the live working tree: %v", err)
		}
		runTestGit(t, git, installed, "config", "--unset", "core.worktree")
		runTestGit(t, git, installed, "sparse-checkout", "set", "--cone", "other")
		rows, err = store.InstallPreparedRecommendationsFrom(t.Context(), routes, build)
		if err != nil || len(rows) != 1 || rows[0].Commit != target || rows[0].Status != "updated" {
			t.Fatalf("prepared result: %+v, %v", rows, err)
		}
		if head, err := CleanRepositoryHead(t.Context(), installed); err != nil || head != target || read(filepath.Join(installed, "new.txt")) != "maintainer 0.9.3" {
			t.Fatalf("target: %s %v", head, err)
		}
		if _, err := os.Stat(filepath.Join(installed, "extra.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unexpected selected extra file", err)
		}
		if read(filepath.Join(installed, "feature/module.ts")) != "export const selected = true;\n" {
			t.Fatal("selected module content differs from target")
		}
		if runTestGit(t, git, installed, "remote", "get-url", "origin") != origin || runTestGit(t, git, sentinel, "remote", "get-url", "origin") != sentinelOrigin || runTestGit(t, git, sentinel, "rev-parse", "HEAD") != sentinelHead || read(filepath.Join(sentinel, "private.txt")) != "unselected" {
			t.Fatal("upstream or sentinel changed")
		}
		t.Logf("after: selected=%s file=%q unchanged sentinel=%s; both origins unchanged", target, read(filepath.Join(installed, "new.txt")), sentinelHead)
	})
}

func TestRecommendedSourcesUseAuthenticatedMirrorAndPreserveProtection(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, working := t.TempDir(), t.TempDir()
	runTestGit(t, git, working, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = 'Mirror fixture'\n")
	runTestGit(t, git, working, "add", ".")
	runTestGit(t, git, working, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "first")
	runTestGit(t, git, working, "tag", "0.10.1")
	first := runTestGit(t, git, working, "rev-parse", "HEAD")
	bare := filepath.Join(remoteRoot, "acme/demo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, git, "", "clone", "-q", "--bare", working, bare)
	runTestGit(t, git, bare, "update-server-info")
	var upstreamRequests, mirrorRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		http.Error(w, "upstream must not be used", 500)
	}))
	defer upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorRequests.Add(1)
		username, password, ok := r.BasicAuth()
		if !ok || username != "deployer" || password != "peer-password" {
			http.Error(w, "unauthorized", 401)
			return
		}
		http.FileServer(http.Dir(remoteRoot)).ServeHTTP(w, r)
	}))
	defer mirror.Close()
	root := t.TempDir()
	store := newTestStore(t, root)
	store.SetSchemaDeployment(&recordingSchemaHook{})
	store.secrets = testSecretResolver{"peer": "peer-password", "old": "upstream-password"}
	path := filepath.Join(root, "node/kernel/scripts/bootstrap-packages.toml")
	writeFile(t, path, fmt.Sprintf("schema = 1\n[[packages]]\nid = 'acme/demo'\nsource = '%s/acme/demo.git'\nversion = '0.10.1'\n", upstream.URL))
	sources := []RecommendedSource{{PackageID: "acme/demo", Source: mirror.URL + "/acme/demo.git", Secret: "peer", Username: "deployer"}}
	installed := filepath.Join(root, "packages/acme/demo")
	if err := os.MkdirAll(filepath.Dir(installed), 0755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, git, "", "clone", "-q", bare, installed)
	runTestGit(t, git, installed, "remote", "set-url", "origin", upstream.URL+"/acme/demo.git")
	runTestGit(t, git, installed, "config", "--replace-all", "remote.origin.fetch", "+refs/tags/old-only:refs/tags/old-only")
	entry, err := store.SetPackageIndex(t.Context(), PackageIndex{Author: "acme", Repository: "demo", Source: upstream.URL + "/acme/demo.git", Secret: "old"})
	if err != nil {
		t.Fatal(err)
	}
	entry.State, entry.ActiveCommit = "ready", first
	if err := store.index.Put(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	rows, err := store.InstallRecommendedPackagesFrom(t.Context(), sources)
	if err != nil || len(rows) != 1 || rows[0].Status != "updated" || rows[0].Commit != first {
		t.Fatalf("mirror migration = %+v, %v", rows, err)
	}
	entry, _ = store.InspectPackageIndex("acme/demo")
	if entry.Source != upstream.URL+"/acme/demo.git" || entry.Secret != "old" || runTestGit(t, git, installed, "remote", "get-url", "origin") != sources[0].Source {
		t.Fatal("configured upstream or development Git origin changed")
	}
	rows, err = store.InstallRecommendedPackagesFrom(t.Context(), sources)
	if err != nil || rows[0].Status != "unchanged" {
		t.Fatalf("repeated recommendation = %+v, %v", rows, err)
	}
	if upstreamRequests.Load() != 0 || mirrorRequests.Load() == 0 {
		t.Fatal("incorrect Git transport")
	}
	writeFile(t, filepath.Join(installed, "local.txt"), "private work")
	rows, err = store.InstallRecommendedPackagesFrom(t.Context(), sources)
	if err != nil || rows[0].Status != "skipped" || rows[0].Reason != "uncommitted changes" {
		t.Fatalf("local work = %+v, %v", rows, err)
	}
	if err := os.Remove(filepath.Join(installed, "local.txt")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]RecommendedSource{nil, {}, {{PackageID: "acme/other", Source: sources[0].Source}}, {{PackageID: "acme/demo", Source: sources[0].Source, Secret: "missing", Username: "deployer"}}} {
		rows, err := store.InstallRecommendedPackagesFrom(t.Context(), bad)
		if err == nil {
			t.Fatal("invalid/missing transport accepted")
		}
		if len(bad) == 1 && bad[0].Secret == "missing" && (len(rows) != 1 || rows[0].Status != "failed") {
			t.Fatalf("credential failure lost package result: %+v", rows)
		}
	}
	if upstreamRequests.Load() != 0 {
		t.Fatal("failed mirror selection fell back to upstream")
	}
	if err := os.RemoveAll(installed); err != nil {
		t.Fatal(err)
	}
	rows, err = store.InstallRecommendedPackagesFrom(t.Context(), sources)
	if err != nil || rows[0].Status != "installed" {
		t.Fatalf("missing package = %+v, %v", rows, err)
	}
	entry, _ = store.InspectPackageIndex("acme/demo")
	if entry.Source != upstream.URL+"/acme/demo.git" || entry.Secret != "old" {
		t.Fatal("reinstall lost the configured upstream")
	}
}

func TestRecommendedConfigurationValidatesBeforeMutation(t *testing.T) {
	for _, invalid := range []string{
		"schema = 2\n",
		"schema = 1\nunknown = true\n",
		"schema = 1\n[[packages]]\nid = '../bad'\n",
		"schema = 1\n[[packages]]\nid = 'acme/demo'\nsource = 'https://user:secret@example.test/acme/demo.git'\n",
		"schema = 1\n[[packages]]\nid = 'acme/demo'\nsource = 'https://example.test/acme/demo.git'\nversion = 'latest'\n",
		"schema = 1\n" + strings.Repeat("[[packages]]\nid = 'acme/demo'\nsource = 'https://example.test/acme/demo.git'\n", 2),
		"schema = 1\n#" + strings.Repeat("x", manifestLimit),
	} {
		root := t.TempDir()
		store := newTestStore(t, root)
		writeFile(t, filepath.Join(root, "node/kernel/scripts/bootstrap-packages.toml"), invalid)
		if _, err := store.InstallRecommendedPackages(t.Context()); err == nil {
			t.Fatal("invalid recommendation accepted")
		}
		entries, _ := store.ListPackageIndexes()
		if len(entries) != 0 {
			t.Fatal("invalid configuration changed desired state")
		}
	}
}

func TestRecommendedPackagesPreserveLocalAndCustomSources(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	for _, entry := range []PackageIndex{
		{Author: "acme", Repository: "local", Local: true},
		{Author: "acme", Repository: "fork", Source: "https://custom.test/acme/fork.git"},
	} {
		if _, err := store.SetPackageIndex(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "node/kernel/scripts/bootstrap-packages.toml")
	writeFile(t, path, "schema = 1\n[[packages]]\nid = 'acme/local'\nsource = 'https://upstream.test/acme/local.git'\n[[packages]]\nid = 'acme/fork'\nsource = 'https://upstream.test/acme/fork.git'\n")
	rows, err := store.InstallRecommendedPackages(t.Context())
	if err != nil || len(rows) != 2 || rows[0].Status != "skipped" || rows[1].Status != "skipped" || !rows[0].Excluded || !rows[1].Excluded {
		t.Fatalf("local/custom recommendations = %+v, %v", rows, err)
	}
	fork, _ := store.InspectPackageIndex("acme/fork")
	if fork.Source != "https://custom.test/acme/fork.git" {
		t.Fatal("custom source was replaced")
	}
}
