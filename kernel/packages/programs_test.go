package packages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProgramRequiresReadyExactProgram(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "users")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Users\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "add", "program.toml"), "schema = 1\ndescription = \"Add a user\"\ndiscoverable = false\nuui = true\n")
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
	if program.Commit != commit || program.Discoverable || !program.UUI || program.EntrypointURL != "file:///workspace/packages/the8020/users/programs/add/program.ts" {
		t.Fatalf("program = %#v", program)
	}
	choices, err := store.ListPrograms(context.Background())
	if err != nil || len(choices) != 1 || choices[0].ID != program.ID || choices[0].Discoverable || !choices[0].UUI {
		t.Fatalf("program choices=%#v %v", choices, err)
	}

	entry, _, _ := store.index.Get(context.Background(), "the8020/users")
	entry.State = "activating"
	if err := store.index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveProgram(context.Background(), "the8020/users/add"); err == nil || !strings.Contains(err.Error(), "no ready active commit") {
		t.Fatalf("inactive package error = %v", err)
	}
	if choices, err := store.ListPrograms(context.Background()); err != nil || len(choices) != 0 {
		t.Fatalf("inactive program choices=%#v %v", choices, err)
	}
}

func TestProgramUUIFlagIsAnOptionalBoolean(t *testing.T) {
	for _, test := range []struct {
		name    string
		flag    string
		uui     bool
		invalid bool
	}{
		{name: "omitted"},
		{name: "false", flag: "uui = false\n"},
		{name: "true", flag: "uui = true # interactive\n", uui: true},
		{name: "string", flag: "uui = \"true\"\n", invalid: true},
		{name: "number", flag: "uui = 1\n", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "programs", "example", "program.toml"), "schema = 1\ndescription = \"Example\"\n"+test.flag)
			writeFile(t, filepath.Join(root, "programs", "example", "program.ts"), "export default () => {};\n")
			program, err := ValidateProgram(root, "acme/tools", "example", "commit")
			if (err != nil) != test.invalid || (err == nil && program.UUI != test.uui) {
				t.Fatalf("program = %#v; error = %v", program, err)
			}
		})
	}
}

func TestResolveProgramUsesPublishedMetadataAndSharedMount(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "acme", "tools")
	writeFile(t, filepath.Join(packageRoot, "programs", "inspect", "program.toml"), "schema = 1\ndescription = \"Inspect\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "inspect", "program.ts"), "export default () => {};\n")
	// An invocation must not run Git or fingerprint unrelated package contents.
	if err := os.Mkdir(filepath.Join(packageRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, root)
	store.gitPath = filepath.Join(root, "unavailable-git")
	if err := store.index.Put(context.Background(), PackageIndex{
		PackageID: "acme/tools", Author: "acme", Repository: "tools",
		State: "ready", ActiveCommit: "published-commit",
	}); err != nil {
		t.Fatal(err)
	}
	program, err := store.ResolveProgram(context.Background(), "acme/tools/inspect")
	if err != nil {
		t.Fatal(err)
	}
	if program.Commit != "published-commit" || program.EntrypointURL != "file:///workspace/packages/acme/tools/programs/inspect/program.ts" {
		t.Fatalf("program = %#v", program)
	}
	entries, err := os.ReadDir(store.PackagesRoot())
	if err != nil || len(entries) != 1 || entries[0].Name() != "acme" {
		t.Fatalf("resolution created package artifacts: entries=%v error=%v", entries, err)
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
