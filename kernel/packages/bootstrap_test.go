package packages

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

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
	runTestGit(t, gitPath, packageRoot, "remote", "add", "origin", "https://github.com/the8020/demo.git")
	runTestGit(t, gitPath, packageRoot, "config", "--local", bootstrapRequestedTagConfig, "0.1.7")

	store := newTestStore(t, root)
	if _, err := store.stageInstalled(context.Background(), map[string]string{"the8020/demo": commit}); err != nil {
		t.Fatal(err)
	}
	entry, exists, err := store.index.Get(context.Background(), "the8020/demo")
	if err != nil || !exists {
		t.Fatalf("bootstrap package index exists=%t err=%v", exists, err)
	}
	if entry.Source != "https://github.com/the8020/demo.git" || entry.Tag != "0.1.7" || entry.Commit != "" || entry.Local {
		t.Fatalf("bootstrap package index = %#v", entry)
	}
}
