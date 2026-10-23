package packages

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestValidateInstalledAllowsUninstalledIntentAfterRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := newTestStore(t, root)
	index := store.index.(*memoryPackageIndexStore)
	index.entries["example/installed"] = PackageIndex{PackageID: "example/installed", State: "ready", ActiveCommit: "installed"}
	commits := map[string]string{"example/installed": "installed"}
	path := filepath.Join(root, "packages", "example", "pending")
	for _, state := range []string{"desired", "failed"} {
		index.entries["example/pending"] = PackageIndex{PackageID: "example/pending", State: state}
		for _, empty := range []bool{false, true} {
			if empty {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.ValidateInstalled(ctx, commits); err != nil {
				t.Fatalf("state=%s empty=%t: %v", state, empty, err)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeFile(t, filepath.Join(path, "unexpected"), "corrupt source")
	if err := store.ValidateInstalled(ctx, commits); err == nil {
		t.Fatal("nonempty uninitialized source was accepted")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []PackageIndex{
		{PackageID: "example/pending", State: "activating"},
		{PackageID: "example/pending", State: "failed", ActiveCommit: "lost"},
	} {
		index.entries[entry.PackageID] = entry
		if err := store.ValidateInstalled(ctx, commits); err == nil {
			t.Fatalf("unsettled or lost active source accepted: %#v", entry)
		}
	}
	delete(index.entries, "example/pending")
	commits["example/unindexed"] = "unexpected"
	if err := store.ValidateInstalled(ctx, commits); err == nil {
		t.Fatal("unindexed installed source was accepted")
	}
}

func TestStageInstalledPreservesResolvedBootstrapTag(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "demo")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\n")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", packageRoot)
	runTestGit(t, gitPath, packageRoot, "add", "package.toml")
	runTestGit(t, gitPath, packageRoot, "commit", "-q", "-m", "initial")
	commit := runTestGit(t, gitPath, packageRoot, "rev-parse", "HEAD")
	runTestGit(t, gitPath, packageRoot, "tag", "0.1.7")
	runTestGit(t, gitPath, packageRoot, "remote", "add", "origin", "https://example.test/the8020/demo.git")
	runTestGit(t, gitPath, packageRoot, "config", "--local", bootstrapRequestedTagConfig, "0.1.7")

	for _, scheme := range []string{"http", "https"} {
		source := scheme + "://example.test/the8020/demo.git"
		runTestGit(t, gitPath, packageRoot, "remote", "set-url", "origin", source)
		store := newTestStore(t, root)
		if _, err := store.stageInstalled(context.Background(), map[string]string{"the8020/demo": commit}); err != nil {
			t.Fatal(err)
		}
		entry, exists, err := store.index.Get(context.Background(), "the8020/demo")
		if err != nil || !exists {
			t.Fatalf("bootstrap package index exists=%t err=%v", exists, err)
		}
		if entry.Source != source || entry.Tag != "0.1.7" || entry.Commit != "" || entry.Local {
			t.Fatalf("bootstrap package index = %#v", entry)
		}
	}
}
