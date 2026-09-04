package packages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProgramRequiresReadyExactProgram(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "users")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Users\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.toml"), "schema = 1\ndescription = \"Add a user\"\ndiscoverable = false\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.ts"), "export default () => {};\n")
	commit, err := FingerprintPackage(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, root)
	if err := store.index.Put(context.Background(), PackageIndex{
		PackageID: "the8020/users", Author: "the8020", Repository: "users",
		State: "ready", ActiveCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	program, err := store.ResolveProgram(context.Background(), "the8020/users/add")
	if err != nil {
		t.Fatal(err)
	}
	if program.Commit != commit || program.Discoverable || program.EntrypointURL != "file:///workspace/packages/the8020/users/programs/add/program.ts" {
		t.Fatalf("program = %#v", program)
	}

	entry, _, _ := store.index.Get(context.Background(), "the8020/users")
	entry.State = "activating"
	if err := store.index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveProgram(context.Background(), "the8020/users/add"); err == nil || !strings.Contains(err.Error(), "no ready active commit") {
		t.Fatalf("inactive package error = %v", err)
	}
}

func TestSnapshotProgramKeepsExactSourceUntilCleanup(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "users")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Users\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.toml"), "schema = 1\ndescription = \"Add a user\"\ndiscoverable = false\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.ts"), "export default () => 'first';\n")
	writeFile(t, filepath.Join(packageRoot, "vendor", "library", ".git", "objects", "unneeded"), "source-control metadata\n")
	commit, err := FingerprintPackage(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, root)
	if err := store.index.Put(context.Background(), PackageIndex{
		PackageID: "the8020/users", Author: "the8020", Repository: "users",
		State: "ready", ActiveCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	program, cleanup, err := store.SnapshotProgram(context.Background(), "the8020/users/add")
	if err != nil {
		t.Fatal(err)
	}
	if program.PackageRoot == packageRoot || !strings.Contains(program.PackageRoot, ".cbus-programs") {
		t.Fatalf("snapshot root = %q", program.PackageRoot)
	}
	if _, err := os.Stat(filepath.Join(program.PackageRoot, "vendor", "library", ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source-control metadata entered runtime snapshot: %v", err)
	}
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.ts"), "export default () => 'changed';\n")
	data, err := os.ReadFile(program.HostPath)
	if err != nil || string(data) != "export default () => 'first';\n" {
		t.Fatalf("snapshot changed with active source = %q, %v", data, err)
	}
	if err := os.RemoveAll(packageRoot); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(program.HostPath)
	if err != nil || string(data) != "export default () => 'first';\n" {
		t.Fatalf("snapshot source = %q, %v", data, err)
	}
	snapshotRoot := filepath.Dir(program.PackageRoot)
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot remained after cleanup: %v", err)
	}
}

func TestValidateProgramRejectsTraversalInvalidManifestAndSymlinks(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	writeFile(t, filepath.Join(packageRoot, "programs", "safe", "program.toml"), "schema = 1\ndescription = \"Safe\"\nentrypoint = \"../escape.ts\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "escape.ts"), "export default () => {};\n")
	if _, err := ValidateProgram(packageRoot, "acme/tools", "safe", "commit"); err == nil || !strings.Contains(err.Error(), "canonical relative path") {
		t.Fatalf("traversal error = %v", err)
	}

	writeFile(t, filepath.Join(packageRoot, "programs", "safe", "program.toml"), "schema = 2\ndescription = \"Safe\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "safe", "program.ts"), "export default () => {};\n")
	if _, err := ValidateProgram(packageRoot, "acme/tools", "safe", "commit"); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("manifest error = %v", err)
	}

	writeFile(t, filepath.Join(packageRoot, "programs", "safe", "program.toml"), "schema = 1\ndescription = \"Safe\"\n")
	if err := os.Remove(filepath.Join(packageRoot, "programs", "safe", "program.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(packageRoot, "programs", "escape.ts"), filepath.Join(packageRoot, "programs", "safe", "program.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateProgram(packageRoot, "acme/tools", "safe", "commit"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestParseProgramIDRejectsWrongDepth(t *testing.T) {
	for _, value := range []string{"the8020/users", "the8020/users/add/extra", "the8020/../add", "the8020/users/bad name"} {
		if _, _, err := ParseProgramID(value); err == nil {
			t.Fatalf("unsafe program ID %q accepted", value)
		}
	}
}
