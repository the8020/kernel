package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/deployment"
	programrunner "the8020/kernel/execution/programs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
)

type fakePackages struct {
	root    string
	entries []workspacepackages.PackageIndex
	errors  map[string]error
}

func (f *fakePackages) ListPackageIndexes() ([]workspacepackages.PackageIndex, error) {
	return append([]workspacepackages.PackageIndex(nil), f.entries...), nil
}

func (f *fakePackages) InspectPackageIndex(id string) (workspacepackages.PackageIndex, error) {
	for _, entry := range f.entries {
		if entry.PackageID == id {
			return entry, nil
		}
	}
	return workspacepackages.PackageIndex{}, os.ErrNotExist
}

func (f *fakePackages) ActivatedPackageCommit(_ context.Context, packageID string) (string, error) {
	if err := f.errors[packageID]; err != nil {
		return "", err
	}
	for _, entry := range f.entries {
		if entry.PackageID == packageID {
			return entry.ActiveCommit, nil
		}
	}
	return "", os.ErrNotExist
}

func (f *fakePackages) PackagesRoot() string { return f.root }

type fakePrograms struct {
	mu             sync.Mutex
	programID      string
	expectedCommit string
	arguments      []any
	secrets        map[string]string
	err            error
}

func (f *fakePrograms) Run(_ context.Context, programID, expectedCommit string, arguments []any, secrets map[string]string) (programrunner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.programID = programID
	f.expectedCommit = expectedCommit
	f.arguments = append([]any(nil), arguments...)
	f.secrets = map[string]string{}
	for name, value := range secrets {
		f.secrets[name] = value
	}
	return programrunner.Result{Value: map[string]any{"ok": true}}, f.err
}

func TestPackageCommandPreservesStructuredProgramErrors(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/services", "scale.toml", "services.scale", "scale", "")
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{
		PackageID: "the8020/services", State: "ready", ActiveCommit: "commit",
	}}}
	programs := &fakePrograms{err: &supervisor.ResponseError{
		Code: "invalid_arguments", Message: "invalid scale", Details: map[string]any{"field": "maximum_workers"},
	}}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, programs, registry)
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := registry.Execute(context.Background(), core.Request{
		ProtocolVersion: core.ProtocolVersion, CommandID: registry.Catalog().Commands[0].ID,
	})
	if response.Error == nil || response.Error.Code != "invalid_arguments" || response.Error.Message != "invalid scale" || response.Error.Details["field"] != "maximum_workers" {
		t.Fatalf("response = %#v", response)
	}
}

func TestReindexUsesExplicitNamesAndPreservesProgramDispatch(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/users", "Arbitrary filename.toml", "users.sessions.revoke", "revoke", "")
	writeCommandPackage(t, root, "acme/billing", ".hidden.toml", "payments.refund", "refund", "")
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{
		{PackageID: "the8020/users", State: "ready", ActiveCommit: "commit-users"},
		{PackageID: "acme/billing", State: "ready", ActiveCommit: "commit-billing"},
		{PackageID: "the8020/inactive", State: "activating", ActiveCommit: "ignored"},
	}}
	programs := &fakePrograms{}
	registry := core.NewRegistry(nil)
	indexer, err := New(packages, programs, registry)
	if err != nil {
		t.Fatal(err)
	}
	report, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Commands != 2 || !reflect.DeepEqual(report.Packages, []string{"acme/billing", "the8020/users"}) {
		t.Fatalf("report = %#v", report)
	}
	catalog := registry.Catalog()
	if len(catalog.Commands) != 2 || catalog.Commands[0].Name != "payments.refund" || catalog.Commands[1].Name != "users.sessions.revoke" {
		t.Fatalf("catalog = %#v", catalog.Commands)
	}
	command := catalog.Commands[1]
	response := registry.Execute(context.Background(), core.Request{
		ProtocolVersion: core.ProtocolVersion, CommandID: command.ID,
		Argv: []string{"Alice Smith", "--", "--literal"},
	})
	if !response.Success {
		t.Fatalf("response = %#v", response)
	}
	programs.mu.Lock()
	defer programs.mu.Unlock()
	if programs.programID != "the8020/users/revoke" || programs.expectedCommit != "commit-users" || !reflect.DeepEqual(programs.arguments, []any{"Alice Smith", "--", "--literal"}) {
		t.Fatalf("program=%q commit=%q arguments=%#v", programs.programID, programs.expectedCommit, programs.arguments)
	}
}

func TestReindexChangesRevisionForPackageCommitAndRemovesInactiveCommands(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/users", "list.toml", "users.list", "list", "")
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{
		PackageID: "the8020/users", State: "ready", ActiveCommit: "first",
	}}}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, &fakePrograms{}, registry)
	first, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstCommand := registry.Catalog().Commands[0]
	packages.entries[0].ActiveCommit = "second"
	second, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondCommand := registry.Catalog().Commands[0]
	if second.Revision == first.Revision || secondCommand.ID == firstCommand.ID || secondCommand.Origin.Commit != "second" {
		t.Fatalf("first=%#v second=%#v", firstCommand, secondCommand)
	}
	packages.entries = nil
	removed, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed.Revision == second.Revision || removed.Commands != 0 || len(registry.Catalog().Commands) != 0 {
		t.Fatalf("removed report=%#v catalog=%#v", removed, registry.Catalog())
	}
}

func TestReindexOmitsBrokenAndCollidingFragments(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/good", "list.toml", "good.list", "list", "")
	writeCommandPackage(t, root, "the8020/acme", "one.toml", "billing.refund", "refund", "")
	writeCommandPackage(t, root, "acme/billing", "two.toml", "billing.refund", "refund", "")
	writeCommandPackage(t, root, "the8020/broken", "bad.toml", "broken.bad", "missing", "")
	writeCommandPackage(t, root, "acme/duplicate", "a.toml", "duplicate.list", "list", "")
	writeCommandPackage(t, root, "acme/duplicate", "b.toml", "duplicate.list", "list", "")
	if err := os.Remove(filepath.Join(root, "the8020", "broken", "programs", "missing", "program.ts")); err != nil {
		t.Fatal(err)
	}
	packages := &fakePackages{root: root}
	for _, item := range []struct{ id, commit string }{
		{"the8020/good", "good"}, {"the8020/acme", "first"},
		{"acme/billing", "third"}, {"the8020/broken", "broken"},
		{"acme/duplicate", "duplicate"},
	} {
		packages.entries = append(packages.entries, workspacepackages.PackageIndex{PackageID: item.id, State: "ready", ActiveCommit: item.commit})
	}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, &fakePrograms{}, registry)
	report, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Commands != 1 || len(report.Diagnostics) != 4 || registry.Catalog().Commands[0].Name != "good.list" {
		t.Fatalf("report=%#v catalog=%#v", report, registry.Catalog())
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.PackageID == "acme/duplicate" && (!strings.Contains(diagnostic.Message, "a.toml") || !strings.Contains(diagnostic.Message, "b.toml")) {
			t.Fatalf("duplicate diagnostic = %#v", diagnostic)
		}
	}
	// Fix only one of the cross-package declarations. Its previously colliding
	// peer must reappear from the cache without rediscovering that package.
	writeCommandPackage(t, root, "the8020/acme", "one.toml", "billing.credit", "refund", "")
	report, err = indexer.Reindex(context.Background(), "the8020/acme")
	if err != nil || report.Commands != 3 || len(report.Diagnostics) != 2 {
		t.Fatalf("resolved collision report=%#v error=%v", report, err)
	}
}

func TestReservedKernelNameAndManifestValidation(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/kernel", "arbitrary.toml", "kernel.restart", "restart", "")
	item, err := (&Indexer{}).discoverPackage(filepath.Join(root, "the8020", "kernel"), "the8020/kernel", "commit")
	if err == nil || item.registrations != nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved error = %v", err)
	}

	manifest := filepath.Join(root, "the8020", "kernel", "cbus", "commands", "arbitrary.toml")
	writeFile(t, manifest, "version = 1\ncommand = \"example.restart\"\nprogram = \"restart\"\nsummary = \"Restart\"\nrestart_behavior = \"none\"\nunknown = true\n")
	if _, err := (&Indexer{}).discoverPackage(filepath.Join(root, "the8020", "kernel"), "the8020/kernel", "commit"); err == nil {
		t.Fatalf("strict manifest error = %v", err)
	}
}

func TestCandidateValidationRejectsInvalidProgramBeforeActivation(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	writeFile(t, filepath.Join(stage, "package.toml"), "schema = 1\ndescription = \"Candidate\"\n")
	writeFile(t, filepath.Join(stage, "cbus", "commands", "arbitrary.toml"), "version = 1\ncommand = \"users.add\"\nprogram = \"add\"\nsummary = \"Add\"\nrestart_behavior = \"none\"\n")
	packages := &fakePackages{root: root}
	indexer, _ := New(packages, &fakePrograms{}, core.NewRegistry(nil))
	err := indexer.ValidateCandidates(context.Background(), []deployment.Candidate{{PackageID: "the8020/users", Root: stage, Commit: "candidate"}})
	if err == nil || !strings.Contains(err.Error(), "invalid program") {
		t.Fatalf("candidate error = %v", err)
	}
}

func TestSecureMetadataIsValidatedAndForwardedSeparately(t *testing.T) {
	root := t.TempDir()
	secrets := "\n[[secrets]]\nname = \"password\"\nrequired = true\nprompt = \"Password: \"\nconfirmation_prompt = \"Confirm: \"\nstdin_option = \"password-stdin\"\n"
	writeCommandPackage(t, root, "the8020/users", "add.toml", "users.add", "add", secrets)
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{PackageID: "the8020/users", State: "ready", ActiveCommit: "commit"}}}
	programs := &fakePrograms{}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, programs, registry)
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	command := registry.Catalog().Commands[0]
	missing := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: command.ID, Argv: []string{"alice"}})
	if missing.Error == nil || missing.Error.Code != core.CodeInvalidArguments {
		t.Fatalf("missing secret = %#v", missing)
	}
	unknown := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: command.ID, Secrets: map[string]string{"other": "value"}})
	if unknown.Error == nil || !strings.Contains(unknown.Error.Message, "unknown secure") {
		t.Fatalf("unknown secret = %#v", unknown)
	}
	response := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: command.ID, Argv: []string{"alice"}, Secrets: map[string]string{"password": "private"}})
	if !response.Success {
		t.Fatalf("response = %#v", response)
	}
	programs.mu.Lock()
	defer programs.mu.Unlock()
	if !reflect.DeepEqual(programs.arguments, []any{"alice"}) || programs.secrets["password"] != "private" {
		t.Fatalf("arguments=%#v secrets=%#v", programs.arguments, programs.secrets)
	}
}

func writeCommandPackage(t *testing.T, root, packageID, filename, command, program, extra string) {
	t.Helper()
	parts := strings.Split(packageID, "/")
	packageRoot := filepath.Join(root, parts[0], parts[1])
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Package\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", program, "program.toml"), "schema = 1\ndescription = \"Command program\"\ndiscoverable = false\n")
	writeFile(t, filepath.Join(packageRoot, "programs", program, "program.ts"), "export default () => {};\n")
	writeFile(t, filepath.Join(packageRoot, "cbus", "commands", filename), fmt.Sprintf("version = 1\ncommand = %q\nprogram = %q\nsummary = \"Command\"\nrestart_behavior = \"none\"\n%s", command, program, extra))
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScopedReindexRetainsUnselectedFragmentsAndRemovesSelectedDeclarations(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"acme/one", "acme/two"} {
		writeCommandPackage(t, root, id, "list.toml", strings.ReplaceAll(id, "/", ".")+".list", "list", "")
	}
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{
		{PackageID: "acme/one", State: "ready", ActiveCommit: "first"},
		{PackageID: "acme/two", State: "ready", ActiveCommit: "first"},
	}}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, &fakePrograms{}, registry)
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Corrupt unselected declarations to prove this refresh does no discovery there.
	if err := os.WriteFile(filepath.Join(root, "acme/two/cbus/commands/list.toml"), []byte("bad = ["), 0600); err != nil {
		t.Fatal(err)
	}
	packages.entries[0].ActiveCommit = "second"
	if report, err := indexer.Reindex(context.Background(), "acme/one"); err != nil || report.Commands != 2 || len(report.Diagnostics) != 0 {
		t.Fatalf("report=%#v error=%v", report, err)
	}
	if err := os.Remove(filepath.Join(root, "acme/one/cbus/commands/list.toml")); err != nil {
		t.Fatal(err)
	}
	if report, err := indexer.Reindex(context.Background(), "acme/one"); err != nil || report.Commands != 1 {
		t.Fatalf("removal report=%#v error=%v", report, err)
	}
	if report, err := indexer.Reindex(context.Background()); err != nil || report.Commands != 0 || len(report.Diagnostics) != 1 {
		t.Fatalf("full report=%#v error=%v", report, err)
	}
	packages.entries = nil
	if report, err := indexer.Reindex(context.Background(), "acme/two"); err != nil || len(report.Diagnostics) != 0 {
		t.Fatalf("removed package report=%#v error=%v", report, err)
	}
}

func TestScopedReindexIgnoresFilenameRenameAndReplacesChangedCommand(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "acme/tools", "before.toml", "tools.check", "check", "")
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{
		PackageID: "acme/tools", State: "ready", ActiveCommit: "active",
	}}}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, &fakePrograms{}, registry)
	first, err := indexer.Reindex(context.Background())
	if err != nil || first.Commands != 1 {
		t.Fatalf("first report=%#v error=%v", first, err)
	}
	command := registry.Catalog().Commands[0]
	folder := filepath.Join(root, "acme/tools/cbus/commands")
	if err := os.Rename(filepath.Join(folder, "before.toml"), filepath.Join(folder, "Completely different.toml")); err != nil {
		t.Fatal(err)
	}
	renamed, err := indexer.Reindex(context.Background(), "acme/tools")
	if err != nil || renamed.Commands != 1 || !reflect.DeepEqual(registry.Catalog().Commands[0], command) {
		t.Fatalf("rename report=%#v error=%v catalog=%#v", renamed, err, registry.Catalog())
	}
	writeCommandPackage(t, root, "acme/tools", "Completely different.toml", "diagnostics.run", "check", "")
	changed, err := indexer.Reindex(context.Background(), "acme/tools")
	if err != nil || changed.Revision == first.Revision || changed.Commands != 1 {
		t.Fatalf("changed report=%#v error=%v", changed, err)
	}
	replacement := registry.Catalog().Commands[0]
	if replacement.Name != "diagnostics.run" || replacement.ID == command.ID || !reflect.DeepEqual(replacement.Path, []string{"diagnostics.run"}) {
		t.Fatalf("replacement = %#v", replacement)
	}
	stale := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: command.ID})
	if stale.Success {
		t.Fatalf("removed command still executes: %#v", stale)
	}
}

func TestCommandFieldIsRequiredAndValidated(t *testing.T) {
	for _, test := range []struct{ name, declaration, want string }{
		{"missing", "", "command is required"},
		{"empty", `command = ""`, "command is required"},
		{"whitespace", `command = " tools.check "`, "dot-separated"},
		{"space", `command = "tools check"`, "dot-separated"},
		{"directory", `command = "tools/check"`, "dot-separated"},
		{"uppercase", `command = "Tools.check"`, "dot-separated"},
		{"empty segment", `command = "tools..check"`, "dot-separated"},
		{"leading dot", `command = ".tools"`, "dot-separated"},
		{"trailing dot", `command = "tools."`, "dot-separated"},
		{"kernel", `command = "kernel"`, "reserved"},
		{"kernel child", `command = "kernel.restart"`, "reserved"},
		{"unknown field", "command = \"tools.check\"\nunknown = true", "strict mode"},
		{"oversized", "command = \"tools.check\"\n#" + strings.Repeat("x", commandManifestLimit), "size limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tools.check.toml")
			writeFile(t, path, "version = 1\nprogram = \"check\"\nsummary = \"Check\"\nrestart_behavior = \"none\"\n"+test.declaration+"\n")
			if _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestCommandDirectoryRejectsNestedFilesAndSymlinks(t *testing.T) {
	for _, kind := range []string{"nested", "source", "file symlink", "directory symlink", "parent symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeCommandPackage(t, root, "acme/tools", "valid.toml", "tools.check", "check", "")
			packageRoot := filepath.Join(root, "acme/tools")
			folder := filepath.Join(packageRoot, "cbus/commands")
			want := "symlink"
			switch kind {
			case "nested":
				writeFile(t, filepath.Join(folder, "nested/command.toml"), "version = 1\n")
				want = "flat TOML"
			case "source":
				writeFile(t, filepath.Join(folder, "handler.ts"), "export default () => {};\n")
				want = "flat TOML"
			case "file symlink":
				if err := os.Symlink(filepath.Join(folder, "valid.toml"), filepath.Join(folder, "linked.toml")); err != nil {
					t.Fatal(err)
				}
			default:
				target := folder
				if kind == "parent symlink" {
					target = filepath.Dir(folder)
				}
				if err := os.Rename(target, target+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target+"-real", target); err != nil {
					t.Fatal(err)
				}
			}
			item, err := (&Indexer{}).discoverPackage(packageRoot, "acme/tools", "active")
			if err == nil || !strings.Contains(err.Error(), want) || len(item.registrations) != 0 {
				t.Fatalf("fragment=%#v error=%v, want %q", item, err, want)
			}
		})
	}
}

func TestCandidateValidationRejectsExplicitCommandCollisions(t *testing.T) {
	for _, duplicateInCandidate := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-package=%t", duplicateInCandidate), func(t *testing.T) {
			root := t.TempDir()
			writeCommandPackage(t, root, "acme/active", "one.toml", "tools.check", "check", "")
			writeCommandPackage(t, root, "acme/candidate", "two.toml", "tools.check", "check", "")
			if duplicateInCandidate {
				writeCommandPackage(t, root, "acme/candidate", "three.toml", "tools.check", "check", "")
			}
			packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{
				PackageID: "acme/active", State: "ready", ActiveCommit: "active",
			}}}
			registry := core.NewRegistry(nil)
			indexer, _ := New(packages, &fakePrograms{}, registry)
			before, err := indexer.Reindex(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = indexer.ValidateCandidates(context.Background(), []deployment.Candidate{{
				PackageID: "acme/candidate", Root: filepath.Join(root, "acme/candidate"), Commit: "next",
			}})
			if err == nil || !strings.Contains(err.Error(), `command "tools.check"`) {
				t.Fatalf("candidate collision = %v", err)
			}
			if registry.Catalog().Revision != before.Revision || len(registry.Catalog().Commands) != 1 {
				t.Fatalf("candidate validation changed live catalog: %#v", registry.Catalog())
			}
		})
	}
}
