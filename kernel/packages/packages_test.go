package packages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPackageDiscoveryUsesExactlyTwoFilesystemLevels(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\ndescription = \"Example one\"\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = 1\ndescription = \"Variables\"\n")
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
	if items[2].ID != "the8020/demo" || !items[2].Valid || items[2].Description != "Example one" || items[2].ServiceCount != 1 {
		t.Fatalf("valid package = %#v", items[2])
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
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.toml"), `schema = 1
description = "Valid service"
[execution]
mode = "persistent"
[access]
mode = "authenticated"
`)
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.ts"), "export default {};\n")
	writeFile(t, filepath.Join(packageRoot, "services", "broken", "service.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(packageRoot, "programs", "dashboard", "program.toml"), `schema = 1
description = "Dashboard"
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
	if len(summaries) != 1 || summaries[0].ID != "the8020/demo" || summaries[0].ServiceCount != 2 {
		t.Fatalf("package summaries = %#v", summaries)
	}
	if summaries[0].Services != nil || summaries[0].Programs != nil || summaries[0].Files != nil {
		t.Fatalf("package list performed detail inspection: %#v", summaries[0])
	}

	item, err := store.InspectPackage("the8020/demo")
	if err != nil {
		t.Fatal(err)
	}
	if !item.Valid || item.Description != "Example package" || item.DocumentationURL != "https://example.test/docs" || item.License != "Apache-2.0" {
		t.Fatalf("package metadata = %#v", item)
	}
	if len(item.Services) != 2 || item.Services[0].ID != "the8020/demo/broken" || item.Services[0].Valid || item.Services[1].ID != "the8020/demo/valid" || !item.Services[1].Valid {
		t.Fatalf("package services = %#v", item.Services)
	}
	if item.Services[1].ExecutionMode != ExecutionModePersistent || item.Services[1].AccessMode != AccessModeAuthenticated || item.Services[1].Entrypoint != "services/valid/service.ts" {
		t.Fatalf("valid service metadata = %#v", item.Services[1])
	}
	if len(item.Programs) != 2 || item.Programs[0].ID != "the8020/demo/broken" || item.Programs[0].Valid || item.Programs[1].ID != "the8020/demo/dashboard" || !item.Programs[1].Valid || item.Programs[1].Discoverable {
		t.Fatalf("package programs = %#v", item.Programs)
	}
	if item.Programs[1].Entrypoint != "program.ts" || item.Programs[1].DefaultLayout != "layouts/main.json" {
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

func TestServiceDiscoveryReportsInvalidNamesMissingEntrypointsAndExplicitEntrypoints(t *testing.T) {
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "demo")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Package\"\n")
	writeFile(t, filepath.Join(packageRoot, "services", "ignored", "README.md"), "no manifest")
	writeFile(t, filepath.Join(packageRoot, "services", "bad service", "service.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(packageRoot, "services", "missing", "service.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(packageRoot, "services", "explicit", "service.toml"), "schema = 1\nentrypoint = \"main.ts\"\n")
	writeFile(t, filepath.Join(packageRoot, "services", "explicit", "main.ts"), "export default {};\n")

	store := newTestStore(t, root)
	items, err := store.ListServices()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("services = %#v, want only manifest-backed entries", items)
	}
	byID := make(map[string]Service, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	if item := byID["the8020/demo/bad service"]; item.Valid || len(item.ValidationErrors) == 0 {
		t.Fatalf("invalid service name = %#v", item)
	}
	if item := byID["the8020/demo/missing"]; item.Valid || !strings.Contains(strings.Join(item.ValidationErrors, " "), "service.ts") {
		t.Fatalf("missing default entrypoint = %#v", item)
	}
	if item := byID["the8020/demo/explicit"]; !item.Valid || !strings.HasSuffix(item.Entrypoint, filepath.Join("explicit", "main.ts")) {
		t.Fatalf("explicit entrypoint = %#v", item)
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
	writeFile(t, filepath.Join(outside, "service.toml"), "schema = 1\n")
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
	if len(escapedChildren.Services) != 1 || escapedChildren.Services[0].Valid || !strings.Contains(strings.Join(escapedChildren.Services[0].ValidationErrors, " "), "outside") {
		t.Fatalf("escaped service manifest = %#v", escapedChildren.Services)
	}
	if len(escapedChildren.Programs) != 1 || escapedChildren.Programs[0].Valid || !strings.Contains(strings.Join(escapedChildren.Programs[0].ValidationErrors, " "), "outside") {
		t.Fatalf("escaped program manifest = %#v", escapedChildren.Programs)
	}
}

func TestServiceDiscoveryDefaultsIdentityAndDesiredStatePrecedence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\ndescription = \"Package\"\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), `schema = 1
description = "Variable service"
[execution]
concurrency_per_worker = 20
[scaling]
replicas_min = 1
replicas_max = 3
workers_per_replica_min = 2
workers_per_replica_max = 6
target_utilization = 0.6
[placement]
sandbox_group = "proof"
[openapi]
title = "Variables"
version = "1.0.0"
`)
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts"), "export default {};\n")
	writeFile(t, filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml"), `schema = 1
enabled = true
generation = 7
[execution]
concurrency_per_worker = 24
keep_alive = "2m"
[scaling]
replicas_min = 2
replicas_max = 4
workers_per_replica_min = 4
workers_per_replica_max = 8
target_utilization = 0.75
[placement]
sandbox_group = "shared-examples"
`)

	store := newTestStore(t, root)
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if definition.Identity.ServiceID() != "the8020/demo/variables" || definition.Identity.CanonicalBasePath() != "/the8020/demo/variables" {
		t.Fatalf("identity = %#v", definition.Identity)
	}
	if definition.Service.Entrypoint != "service.ts" || definition.EntrypointURL != "file:///workspace/packages/the8020/demo/services/variables/service.ts" {
		t.Fatalf("entrypoint = %q (%q)", definition.EntrypointPath, definition.EntrypointURL)
	}
	if !definition.StateExists || !definition.State.Enabled || definition.State.Generation != 7 {
		t.Fatalf("state = %#v", definition.State)
	}
	if definition.Effective.Scaling.ReplicasMinimum != 2 || definition.Effective.Scaling.ReplicasMaximum != 4 {
		t.Fatalf("replicas = %#v", definition.Effective.Scaling)
	}
	if definition.Effective.Scaling.WorkersPerReplicaMinimum != 4 || definition.Effective.Scaling.WorkersPerReplicaMaximum != 8 || definition.Effective.Execution.ConcurrencyPerWorker != 24 {
		t.Fatalf("workers/execution = %#v %#v", definition.Effective.Scaling, definition.Effective.Execution)
	}
	if definition.Effective.Timeouts.Request != 30*time.Second || definition.Effective.Timeouts.Drain != 30*time.Second || definition.Effective.Placement.SandboxGroup != "shared-examples" || definition.Effective.Scaling.TargetUtilization != 0.75 {
		t.Fatalf("effective = %#v", definition.Effective)
	}
	services, err := store.ListServices()
	if err != nil || len(services) != 1 || !services[0].Valid || services[0].ID != "the8020/demo/variables" || services[0].CanonicalBasePath != "/the8020/demo/variables" {
		t.Fatalf("services=%#v err=%v", services, err)
	}
}

func TestFirstDiscoveryCreatesDisabledDesiredState(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if !definition.StateExists || definition.State.Enabled || definition.State.Generation != 0 {
		t.Fatalf("state = %#v exists=%t", definition.State, definition.StateExists)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml")); err != nil {
		t.Fatalf("discovery did not create desired state: %v", err)
	}
}

func TestServiceValidationRejectsEntrypointTraversalInvalidTOMLAndDefaults(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		contains string
	}{
		{"traversal", "schema = 1\nentrypoint = \"../outside.ts\"\n", "canonical relative path"},
		{"invalid TOML", "schema = [\n", "toml"},
		{"invalid scaling bounds", "schema = 1\n[scaling]\nworkers_per_replica_min = 4\nworkers_per_replica_max = 3\n", "minimum <= maximum"},
		{"invalid execution mode", "schema = 1\n[execution]\nmode = \"batch\"\n", "execution.mode"},
		{"invalid concurrency", "schema = 1\n[execution]\nconcurrency_per_worker = 0\n", "concurrency_per_worker"},
		{"invalid keep alive", "schema = 1\n[execution]\nmode = \"persistent\"\nkeep_alive = \"forever\"\n", "keep_alive"},
		{"stateless keep alive", "schema = 1\n[execution]\nmode = \"stateless\"\nkeep_alive = \"2m\"\n", "only for persistent"},
		{"invalid utilization", "schema = 1\n[scaling]\ntarget_utilization = 1.1\n", "target_utilization"},
		{"invalid sandbox group", "schema = 1\n[placement]\nsandbox_group = \" bad \"\n", "sandbox_group"},
		{"invalid access mode", "schema = 1\n[access]\nmode = \"private\"\n", "access.mode"},
		{"invalid unauthenticated action", "schema = 1\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"query-redirect\"\n", "reject or redirect"},
		{"invalid reject status", "schema = 1\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"reject\"\nstatus = 302\n", "reject status"},
		{"invalid redirect status", "schema = 1\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"redirect\"\nstatus = 401\nredirect_url = \"/login\"\n", "redirect status"},
		{"invalid redirect URL", "schema = 1\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"redirect\"\nredirect_url = \"/login\\nunsafe\"\n", "redirect_url"},
		{"legacy affinity rejected", "schema = 1\n[routing]\naffinity = [\"auth.user_id\"]\n", "strict mode"},
		{"legacy session rejected", "schema = 1\n[session]\ndisconnect_grace = \"2m\"\n", "strict mode"},
		{"unknown identity", "schema = 1\nservice_id = \"wrong\"\n", "strict mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\n")
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), test.manifest)
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts"), "export default {};\n")
			store := newTestStore(t, root)
			_, err := store.ReadService("the8020/demo/variables")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.contains)) {
				t.Fatalf("error = %v, want %q", err, test.contains)
			}
		})
	}
}

func TestServiceExecutionAndAccessPoliciesDefaultAndNormalize(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "public", "service.ts")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "protected", "service.toml"), `schema = 1
[execution]
mode = "persistent"
concurrency_per_worker = 1
keep_alive = "2m"
[scaling]
replicas_min = 2
replicas_max = 10
workers_per_replica_min = 1
workers_per_replica_max = 50
target_utilization = 0.7
[placement]
sandbox_group = "interactive"
[access]
mode = "authenticated"
[access.unauthenticated]
action = "redirect"
redirect_url = "https://identity.example.test/login?return=static"
`)
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "protected", "service.ts"), "export default {};\n")
	store := newTestStore(t, root)
	public, err := store.ReadService("the8020/demo/public")
	if err != nil {
		t.Fatal(err)
	}
	if public.Service.Execution.Mode != ExecutionModeStateless || public.Service.Access.Mode != AccessModePublic {
		t.Fatalf("public defaults = %#v", public.Service)
	}
	protected, err := store.ReadService("the8020/demo/protected")
	if err != nil {
		t.Fatal(err)
	}
	policy := protected.Service.Access.Unauthenticated
	if protected.Service.Execution.Mode != ExecutionModePersistent || protected.Service.Access.Mode != AccessModeAuthenticated || policy.Action != UnauthenticatedRedirect || policy.Status != 302 || policy.RedirectURL != "https://identity.example.test/login?return=static" {
		t.Fatalf("protected policy = %#v", protected.Service)
	}
	if protected.Effective.Execution.ConcurrencyPerWorker != 1 || protected.Effective.Execution.KeepAlive != 2*time.Minute || protected.Effective.Placement.SandboxGroup != "interactive" || protected.Effective.Scaling.ReplicasMinimum != 2 {
		t.Fatalf("protected execution/scaling/placement = %#v", protected.Effective)
	}
}

func TestMutateStateIsAtomicMonotonicAndSerialized(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	const mutations = 12
	var wait sync.WaitGroup
	errorsByMutation := make(chan error, mutations)
	generations := make(chan uint64, mutations)
	for index := 0; index < mutations; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			state, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *DesiredServiceState) error {
				state.Enabled = true
				return nil
			})
			if err == nil {
				generations <- state.Generation
			}
			errorsByMutation <- err
		}()
	}
	wait.Wait()
	close(errorsByMutation)
	close(generations)
	for err := range errorsByMutation {
		if err != nil {
			t.Fatal(err)
		}
	}
	var values []int
	for generation := range generations {
		values = append(values, int(generation))
	}
	sort.Ints(values)
	for index, generation := range values {
		if generation != index+1 {
			t.Fatalf("generations = %#v", values)
		}
	}
	state, exists, err := store.ReadState("the8020/demo/variables")
	if err != nil || !exists || !state.Enabled || state.Generation != mutations {
		t.Fatalf("state=%#v exists=%t err=%v", state, exists, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml"))
	if err != nil || strings.Contains(string(data), ".state-") {
		t.Fatalf("state file invalid: %q err=%v", data, err)
	}
}

func TestMutateStateCanRepairInvalidEffectiveDesiredState(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	writeFile(t, filepath.Join(root, "state", "services", "the8020", "demo", "variables", "state.toml"), `schema = 1
enabled = true
generation = 8
[scaling]
workers_per_replica_min = 7
workers_per_replica_max = 3
`)
	store := newTestStore(t, root)
	state, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *DesiredServiceState) error {
		minimum, maximum := 1, 4
		state.Scaling.WorkersPerReplicaMinimum = &minimum
		state.Scaling.WorkersPerReplicaMaximum = &maximum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 9 || state.Scaling.WorkersPerReplicaMinimum == nil || *state.Scaling.WorkersPerReplicaMinimum != 1 {
		t.Fatalf("repaired state = %#v", state)
	}
}

func TestMutateStateRejectsStateSymlinkBeforeWritingOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	if err := os.MkdirAll(filepath.Join(root, "state", "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "state", "services", "the8020")); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, root)
	_, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *DesiredServiceState) error {
		state.Enabled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("MutateState() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "demo")); !os.IsNotExist(err) {
		t.Fatalf("mutation wrote outside state root: %v", err)
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
	store, err := New(Config{WorkspaceRoot: root, StateLockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeService(t *testing.T, root, namespace, repository, service, entrypoint string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "services", service, "service.toml"), "schema = 1\nentrypoint = \""+entrypoint+"\"\n")
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
