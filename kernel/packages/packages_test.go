package packages

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = 2\ndescription = \"Variables\"\n")
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

func TestActivatedPackageCommitRequiresCleanExactSource(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "the8020", "demo")
	writeFile(t, filepath.Join(packageRoot, "package.toml"), "schema = 1\ndescription = \"Demo\"\n")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", packageRoot)
	runTestGit(t, gitPath, packageRoot, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, packageRoot, "config", "user.email", "packages@example.test")
	runTestGit(t, gitPath, packageRoot, "add", ".")
	runTestGit(t, gitPath, packageRoot, "commit", "-q", "-m", "initial")
	commit := runTestGit(t, gitPath, packageRoot, "rev-parse", "HEAD")
	store := newTestStore(t, root)
	index := store.index.(*memoryPackageIndexStore)
	if err := index.Put(context.Background(), PackageIndex{
		PackageID: "the8020/demo", Author: "the8020", Repository: "demo",
		State: "ready", ActiveCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	if actual, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err != nil || actual != commit {
		t.Fatalf("activated commit = %q, %v", actual, err)
	}

	writeFile(t, filepath.Join(packageRoot, "draft.ts"), "export const draft = true;\n")
	if _, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty checkout error = %v", err)
	}
	if err := os.Remove(filepath.Join(packageRoot, "draft.ts")); err != nil {
		t.Fatal(err)
	}
	entry, _, _ := index.Get(context.Background(), "the8020/demo")
	entry.ActiveCommit = strings.Repeat("0", len(commit))
	if err := index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatedPackageCommit(context.Background(), "the8020/demo"); err == nil || !strings.Contains(err.Error(), "does not match ready active commit") {
		t.Fatalf("commit mismatch error = %v", err)
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
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.toml"), `schema = 2
description = "Valid service"
[lifecycle]
service_type = "session"
[access]
mode = "authenticated"
`)
	writeFile(t, filepath.Join(packageRoot, "services", "valid", "service.ts"), "export default {};\n")
	writeFile(t, filepath.Join(packageRoot, "services", "broken", "service.toml"), "schema = 99\n")
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
	if item.Services[1].ServiceType != ServiceTypeSession || item.Services[1].AccessMode != AccessModeAuthenticated || item.Services[1].Entrypoint != "services/valid/service.ts" {
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
	writeFile(t, filepath.Join(packageRoot, "services", "bad service", "service.toml"), "schema = 2\n")
	writeFile(t, filepath.Join(packageRoot, "services", "missing", "service.toml"), "schema = 2\n")
	writeFile(t, filepath.Join(packageRoot, "services", "explicit", "service.toml"), "schema = 2\nentrypoint = \"main.ts\"\n")
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
	writeFile(t, filepath.Join(outside, "service.toml"), "schema = 2\n")
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
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), `schema = 2
description = "Variable service"
[scaling]
minimum_workers = 2
maximum_workers = 18
concurrency_per_worker = 20
target_utilization = 0.6
[placement]
sandbox_group = "proof"
minimum_sandboxes = 1
workers_per_sandbox = 6
[openapi]
title = "Variables"
version = "1.0.0"
`)
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts"), "export default {};\n")
	store := newTestStore(t, root)
	minimum, maximum, concurrency, target, minimumSandboxes, workersPerSandbox := 8, 32, 24, 0.75, 2, 8
	workerKeepAlive, sandboxGroup := "2m", "shared-examples"
	if err := store.state.Put("the8020/demo/variables", DesiredServiceState{
		Enabled: true, Generation: 7,
		Scaling:   ScalingOverrides{MinimumWorkers: &minimum, MaximumWorkers: &maximum, ConcurrencyPerWorker: &concurrency, TargetUtilization: &target, WorkerKeepAlive: &workerKeepAlive},
		Placement: PlacementOverrides{SandboxGroup: &sandboxGroup, MinimumSandboxes: &minimumSandboxes, WorkersPerSandbox: &workersPerSandbox},
	}); err != nil {
		t.Fatal(err)
	}
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
	if definition.Effective.Scaling.MinimumWorkers != 8 || definition.Effective.Scaling.MaximumWorkers != 32 {
		t.Fatalf("workers = %#v", definition.Effective.Scaling)
	}
	if definition.Effective.Placement.MinimumSandboxes != 2 || definition.Effective.Placement.WorkersPerSandbox != 8 || definition.Effective.Scaling.ConcurrencyPerWorker != 24 {
		t.Fatalf("scaling/placement = %#v %#v", definition.Effective.Scaling, definition.Effective.Placement)
	}
	if definition.Effective.Timeouts.Request != 30*time.Second || definition.Effective.Timeouts.Drain != 30*time.Second || definition.Effective.Placement.SandboxGroup != "shared-examples" || definition.Effective.Scaling.TargetUtilization != 0.75 {
		t.Fatalf("effective = %#v", definition.Effective)
	}
	services, err := store.ListServices()
	if err != nil || len(services) != 1 || !services[0].Valid || services[0].ID != "the8020/demo/variables" || services[0].CanonicalBasePath != "/the8020/demo/variables" {
		t.Fatalf("services=%#v err=%v", services, err)
	}
}

func TestSourceInspectionDoesNotCreateDesiredState(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	if definition.StateExists || definition.State.Enabled || definition.State.Generation != 0 {
		t.Fatalf("state = %#v exists=%t", definition.State, definition.StateExists)
	}
	if _, exists, err := store.state.Get("the8020/demo/variables"); err != nil || exists {
		t.Fatalf("source inspection mutated desired state: exists=%t err=%v", exists, err)
	}
}

func TestServiceDefaultsUseWorkerScalingAndIndependentKeepalives(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	definition, err := store.ReadService("the8020/demo/variables")
	if err != nil {
		t.Fatal(err)
	}
	effective := definition.Effective
	if effective.Lifecycle.ServiceType != ServiceTypeStateless || effective.Lifecycle.SessionKeepAlive != 10*time.Minute {
		t.Fatalf("lifecycle defaults = %#v", effective.Lifecycle)
	}
	if effective.Scaling.MinimumWorkers != 0 || effective.Scaling.MaximumWorkers != 0 || effective.Scaling.ConcurrencyPerWorker != 32 || effective.Scaling.TargetUtilization != 0.7 || effective.Scaling.WorkerKeepAlive != 2*time.Minute {
		t.Fatalf("scaling defaults = %#v", effective.Scaling)
	}
	if effective.Placement.MinimumSandboxes != 0 || effective.Placement.WorkersPerSandbox != 4 || effective.Placement.SandboxGroup != "" {
		t.Fatalf("placement defaults = %#v", effective.Placement)
	}
	if definition.State.Scaling.MinimumWorkers != nil || definition.State.Scaling.MaximumWorkers != nil || definition.State.Placement.MinimumSandboxes != nil {
		t.Fatalf("package defaults were incorrectly materialized as operator overrides: %#v", definition.State)
	}
}

func TestServicePolicyValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		contains string
	}{
		{"finite maximum below minimum", "[scaling]\nminimum_workers = 5\nmaximum_workers = 4\n", "minimum_workers <= maximum_workers"},
		{"negative maximum", "[scaling]\nmaximum_workers = -1\n", "maximum_workers cannot be negative"},
		{"zero concurrency", "[scaling]\nconcurrency_per_worker = 0\n", "concurrency_per_worker"},
		{"invalid worker keepalive", "[scaling]\nworker_keep_alive = \"never\"\n", "worker_keep_alive"},
		{"negative minimum sandboxes", "[placement]\nminimum_sandboxes = -1\n", "minimum_sandboxes"},
		{"zero workers per sandbox", "[placement]\nworkers_per_sandbox = 0\n", "workers_per_sandbox"},
		{"invalid service type", "[lifecycle]\nservice_type = \"persistent\"\n", "stateless or session"},
		{"invalid session keepalive", "[lifecycle]\nservice_type = \"session\"\nsession_keep_alive = \"forever\"\n", "session_keep_alive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\n")
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = 2\n"+test.body)
			writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts"), "export default {};\n")
			_, err := newTestStore(t, root).ReadService("the8020/demo/variables")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.contains)) {
				t.Fatalf("error = %v, want %q", err, test.contains)
			}
		})
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.toml"), "schema = 2\n[lifecycle]\nservice_type = \"stateless\"\nsession_keep_alive = \"10m\"\n[scaling]\nminimum_workers = 7\nmaximum_workers = 0\n")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "variables", "service.ts"), "export default {};\n")
	if _, err := newTestStore(t, root).ReadService("the8020/demo/variables"); err != nil {
		t.Fatalf("zero maximum must mean unlimited: %v", err)
	}
}

func TestEffectivePolicyValidationIsClassified(t *testing.T) {
	policy := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: time.Minute},
		Scaling: ScalingConfiguration{
			MinimumWorkers: 2, MaximumWorkers: 1, ConcurrencyPerWorker: 1,
			TargetUtilization: 0.7, WorkerKeepAlive: time.Minute,
		},
		Placement:      PlacementConfiguration{WorkersPerSandbox: 1},
		Timeouts:       TimeoutConfiguration{Request: time.Second, Drain: time.Second},
		DependencyMode: "cached-only",
	}
	if err := validateEffective(policy); !errors.Is(err, ErrInvalidServicePolicy) {
		t.Fatalf("validation error = %v", err)
	}
}

func TestServiceValidationRejectsEntrypointTraversalInvalidTOMLAndDefaults(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		contains string
	}{
		{"traversal", "schema = 2\nentrypoint = \"../outside.ts\"\n", "canonical relative path"},
		{"invalid TOML", "schema = [\n", "toml"},
		{"invalid utilization", "schema = 2\n[scaling]\ntarget_utilization = 1.1\n", "target_utilization"},
		{"invalid sandbox group", "schema = 2\n[placement]\nsandbox_group = \" bad \"\n", "sandbox_group"},
		{"invalid access mode", "schema = 2\n[access]\nmode = \"private\"\n", "access.mode"},
		{"invalid unauthenticated action", "schema = 2\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"query-redirect\"\n", "reject or redirect"},
		{"invalid reject status", "schema = 2\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"reject\"\nstatus = 302\n", "reject status"},
		{"invalid redirect status", "schema = 2\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"redirect\"\nstatus = 401\nredirect_url = \"/login\"\n", "redirect status"},
		{"invalid redirect URL", "schema = 2\n[access]\nmode = \"authenticated\"\n[access.unauthenticated]\naction = \"redirect\"\nredirect_url = \"/login\\nunsafe\"\n", "redirect_url"},
		{"obsolete execution section", "schema = 2\n[execution]\nmode = \"persistent\"\n", "strict mode"},
		{"unknown identity", "schema = 2\nservice_id = \"wrong\"\n", "strict mode"},
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

func TestServiceLifecycleAndAccessPoliciesDefaultAndNormalize(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "public", "service.ts")
	writeFile(t, filepath.Join(root, "packages", "the8020", "demo", "services", "protected", "service.toml"), `schema = 2
[lifecycle]
service_type = "session"
session_keep_alive = "2m"
[scaling]
minimum_workers = 2
maximum_workers = 500
concurrency_per_worker = 1
target_utilization = 0.7
[placement]
sandbox_group = "interactive"
minimum_sandboxes = 2
workers_per_sandbox = 50
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
	if public.Service.Lifecycle.ServiceType != ServiceTypeStateless || public.Service.Access.Mode != AccessModePublic {
		t.Fatalf("public defaults = %#v", public.Service)
	}
	protected, err := store.ReadService("the8020/demo/protected")
	if err != nil {
		t.Fatal(err)
	}
	policy := protected.Service.Access.Unauthenticated
	if protected.Service.Lifecycle.ServiceType != ServiceTypeSession || protected.Service.Access.Mode != AccessModeAuthenticated || policy.Action != UnauthenticatedRedirect || policy.Status != 302 || policy.RedirectURL != "https://identity.example.test/login?return=static" {
		t.Fatalf("protected policy = %#v", protected.Service)
	}
	if protected.Effective.Scaling.ConcurrencyPerWorker != 1 || protected.Effective.Lifecycle.SessionKeepAlive != 2*time.Minute || protected.Effective.Placement.SandboxGroup != "interactive" || protected.Effective.Placement.MinimumSandboxes != 2 || protected.Effective.Scaling.MinimumWorkers != 2 {
		t.Fatalf("protected execution/scaling/placement = %#v", protected.Effective)
	}
}

func TestMutateStateIsAtomicMonotonicAndSerialized(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	installTestService(t, store, "the8020/demo/variables")
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
}

func TestMutateStateCanRepairInvalidEffectiveDesiredState(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "the8020", "demo", "variables", "service.ts")
	store := newTestStore(t, root)
	minimum, maximum := 7, 3
	if err := store.state.Put("the8020/demo/variables", DesiredServiceState{
		Enabled: true, Generation: 8,
		Scaling: ScalingOverrides{MinimumWorkers: &minimum, MaximumWorkers: &maximum},
	}); err != nil {
		t.Fatal(err)
	}
	state, err := store.MutateState(context.Background(), "the8020/demo/variables", func(state *DesiredServiceState) error {
		minimum, maximum := 1, 4
		state.Scaling.MinimumWorkers = &minimum
		state.Scaling.MaximumWorkers = &maximum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 9 || state.Scaling.MinimumWorkers == nil || *state.Scaling.MinimumWorkers != 1 {
		t.Fatalf("repaired state = %#v", state)
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
	store, err := New(Config{
		WorkspaceRoot: root,
		StateStore:    &memoryServiceStateStore{states: map[string]DesiredServiceState{}},
		IndexStore:    newMemoryPackageIndexStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func installTestService(t *testing.T, store *Store, serviceID string) {
	t.Helper()
	definition, err := store.ReadService(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.state.Put(serviceID, definition.State); err != nil {
		t.Fatal(err)
	}
}

func writeService(t *testing.T, root, namespace, repository, service, entrypoint string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "package.toml"), "schema = 1\n")
	writeFile(t, filepath.Join(root, "packages", namespace, repository, "services", service, "service.toml"), "schema = 2\nentrypoint = \""+entrypoint+"\"\n")
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
