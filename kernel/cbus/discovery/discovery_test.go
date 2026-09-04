package discovery

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/deployment"
	programrunner "the8020/kernel/execution/programs"
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
	writeCommandPackage(t, root, "the8020/services", "commit", "scale", "scale", "")
	packages := &fakePackages{root: root, entries: []workspacepackages.PackageIndex{{
		PackageID: "the8020/services", State: "ready", ActiveCommit: "commit",
	}}}
	programs := &fakePrograms{err: &programrunner.ExecutionError{
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

func TestReindexDerivesFirstPartyThirdPartyAndNestedNames(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/users", "commit-users", "sessions/revoke", "revoke", "")
	writeCommandPackage(t, root, "acme/billing", "commit-billing", "refund", "refund", "")
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
	if len(catalog.Commands) != 2 || catalog.Commands[0].Name != "acme.billing.refund" || catalog.Commands[1].Name != "users.sessions.revoke" {
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
	writeCommandPackage(t, root, "the8020/users", "first", "list", "list", "")
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
	writeCommandPackage(t, root, "the8020/good", "good", "list", "list", "")
	writeCommandPackage(t, root, "the8020/acme", "first", "billing/refund", "refund", "")
	writeCommandPackage(t, root, "acme/billing", "third", "refund", "refund", "")
	writeCommandPackage(t, root, "the8020/broken", "broken", "bad", "missing", "")
	if err := os.Remove(filepath.Join(root, "the8020", "broken", "programs", "missing", "program.ts")); err != nil {
		t.Fatal(err)
	}
	packages := &fakePackages{root: root}
	for _, item := range []struct{ id, commit string }{
		{"the8020/good", "good"}, {"the8020/acme", "first"},
		{"acme/billing", "third"}, {"the8020/broken", "broken"},
	} {
		packages.entries = append(packages.entries, workspacepackages.PackageIndex{PackageID: item.id, State: "ready", ActiveCommit: item.commit})
	}
	registry := core.NewRegistry(nil)
	indexer, _ := New(packages, &fakePrograms{}, registry)
	report, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Commands != 1 || len(report.Diagnostics) != 3 || registry.Catalog().Commands[0].Name != "good.list" {
		t.Fatalf("report=%#v catalog=%#v", report, registry.Catalog())
	}
}

func TestReservedKernelNameAndManifestValidation(t *testing.T) {
	root := t.TempDir()
	writeCommandPackage(t, root, "the8020/kernel", "commit", "restart", "restart", "")
	item, err := (&Indexer{}).discoverPackage(filepath.Join(root, "the8020", "kernel"), "the8020/kernel", "commit")
	if err == nil || item.registrations != nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved error = %v", err)
	}

	manifest := filepath.Join(root, "the8020", "kernel", "cbus", "commands", "restart", "command.toml")
	writeFile(t, manifest, "version = 1\nprogram = \"restart\"\nsummary = \"Restart\"\nrestart_behavior = \"none\"\nunknown = true\n")
	if _, err := (&Indexer{}).discoverPackage(filepath.Join(root, "the8020", "kernel"), "the8020/kernel", "commit"); err == nil {
		t.Fatalf("strict manifest error = %v", err)
	}
}

func TestCandidateValidationRejectsInvalidProgramBeforeActivation(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	writeFile(t, filepath.Join(stage, "package.toml"), "schema = 1\ndescription = \"Candidate\"\n")
	writeFile(t, filepath.Join(stage, "cbus", "commands", "add", "command.toml"), "version = 1\nprogram = \"add\"\nsummary = \"Add\"\nrestart_behavior = \"none\"\n")
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
	writeCommandPackage(t, root, "the8020/users", "commit", "add", "add", secrets)
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

func writeCommandPackage(t *testing.T, root, packageID, commit, commandPath, program, extra string) {
	t.Helper()
	parts := strings.Split(packageID, "/")
	packageRoot := filepath.Join(root, parts[0], parts[1])
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Package\"\n")
	writeFile(t, filepath.Join(packageRoot, "programs", program, "program.toml"), "schema = 1\ndescription = \"Command program\"\ndiscoverable = false\n")
	writeFile(t, filepath.Join(packageRoot, "programs", program, "program.ts"), "export default () => {};\n")
	writeFile(t, filepath.Join(packageRoot, "cbus", "commands", filepath.FromSlash(commandPath), "command.toml"), "version = 1\nprogram = \""+program+"\"\nsummary = \"Command\"\nrestart_behavior = \"none\"\n"+extra)
	_ = commit
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
