package evaluator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
	workspacepackages "the8020/kernel/packages"
)

type fakeJobs struct {
	calls        []jobs.Options
	malformed    bool
	failAt       int
	dependencies map[string][]string
	descriptor   func(evaluationItem) database.TableDescriptor
}

type guardedCatalog struct {
	PackageCatalog
	err   error
	calls []string
}

func (c *guardedCatalog) ActivatedPackageCommit(_ context.Context, packageID string) (string, error) {
	c.calls = append(c.calls, packageID)
	return "", c.err
}

func (f *fakeJobs) Run(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
	f.calls = append(f.calls, options)
	if f.failAt > 0 && len(f.calls) == f.failAt {
		return jobs.Record{}, errors.New("evaluator failed")
	}
	if f.malformed {
		return jobs.Record{Result: map[string]any{"invalid": true}}, nil
	}
	if len(options.Arguments) != 1 {
		return jobs.Record{}, errors.New("evaluator input was not one argument")
	}
	request := options.Arguments[0].(evaluationRequest)
	tables := make([]database.EvaluatedTable, 0, len(request.Tables))
	for _, item := range request.Tables {
		descriptor := database.TableDescriptor{
			FormatVersion: 1, TableID: item.ExpectedTableID,
			Columns:    []database.ColumnDescriptor{{Name: "id", LogicalType: "text", PrimaryKey: true}},
			PrimaryKey: []string{"id"}, Indexes: []database.IndexDescriptor{},
		}
		if f.descriptor != nil {
			descriptor = f.descriptor(item)
		}
		encoded, _ := json.Marshal(descriptor)
		hash := sha256.Sum256(encoded)
		tables = append(tables, database.EvaluatedTable{
			Descriptor: descriptor, DescriptorJSON: string(encoded), DescriptorHash: hex.EncodeToString(hash[:]),
			SourceModule: item.Module, SourcePackage: item.PackageID, SourceCommit: item.PackageCommit,
			Dependencies: append([]string(nil), item.Dependencies...),
		})
	}
	dependencies := map[string][]string{}
	for _, item := range request.Tables {
		dependencies[item.Module] = append([]string(nil), f.dependencies[item.Module]...)
	}
	return jobs.Record{Result: evaluationResponse{Tables: tables}, ModuleDependencies: dependencies}, nil
}

func testEvaluator(t *testing.T, tableCount int) (*Evaluator, *fakeJobs, *database.Manager, string) {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "acme", "orders")
	if err := os.MkdirAll(filepath.Join(packageRoot, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.toml"), []byte("schema = 1\ndescription = \"Orders\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < tableCount; index++ {
		name := filepath.Join(packageRoot, "tables", tableFile(index))
		if err := os.WriteFile(name, []byte("export default {};\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := workspacepackages.NewCatalog(filepath.Join(root, "packages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	databaseManager := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(root, "system.db"), InstanceRoot: root,
		MaximumOpenConnections: 4, MaximumIdleConnections: 1,
	})
	t.Cleanup(func() { _ = databaseManager.Close() })
	if _, err := databaseManager.InitializeCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner := &fakeJobs{}
	evaluator, err := New(Config{Packages: store, Jobs: runner, Database: databaseManager})
	if err != nil {
		t.Fatal(err)
	}
	databaseManager.SetDefinitionEvaluator(evaluator.Evaluate)
	return evaluator, runner, databaseManager, packageRoot
}

func tableFile(index int) string {
	return "table" + leftPad(index, 4) + ".ts"
}

func leftPad(value, width int) string {
	result := ""
	for value > 0 {
		result = string(rune('0'+value%10)) + result
		value /= 10
	}
	for len(result) < width {
		result = "0" + result
	}
	return result
}

func TestEvaluationBatchesModulesAndReusesOneRelease(t *testing.T) {
	evaluator, runner, _, _ := testEvaluator(t, maximumBatch+1)
	result, err := evaluator.Evaluate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tables) != maximumBatch+1 || len(runner.calls) != 2 {
		t.Fatalf("tables=%d calls=%d", len(result.Tables), len(runner.calls))
	}
	if len(runner.calls[0].CheckModules) != maximumBatch || len(runner.calls[1].CheckModules) != 1 || runner.calls[0].ReleaseID == "" || runner.calls[0].ReleaseID != runner.calls[1].ReleaseID {
		t.Fatalf("batch options = %#v", runner.calls)
	}
	for _, call := range runner.calls {
		if call.DatabaseAccess != "none" || call.Reuse == nil || !*call.Reuse || call.Parallelism != 1 {
			t.Fatalf("unsafe evaluator options = %#v", call)
		}
	}
}

func TestEvaluationRejectsDirtySharedCheckout(t *testing.T) {
	evaluator, runner, _, packageRoot := testEvaluator(t, 1)
	commitRepository(t, packageRoot, "active")
	if err := os.WriteFile(filepath.Join(packageRoot, "draft.ts"), []byte("export const draft = true;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.Evaluate(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty source error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("dirty source reached evaluator: %#v", runner.calls)
	}
}

func TestExplicitSynchronizationRequiresActivatedSourceButCandidateUsesStage(t *testing.T) {
	evaluator, runner, manager, _ := testEvaluator(t, 1)
	ctx := context.Background()
	if _, err := evaluator.SynchronizeAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	manager.SetSourceEvaluator(evaluator.InspectDefinition)
	guard := &guardedCatalog{PackageCatalog: evaluator.packages, err: errors.New("activated checkout is dirty")}
	if err := evaluator.UseActivatedPackages(guard); err != nil {
		t.Fatal(err)
	}
	before := len(runner.calls)
	if _, err := evaluator.SynchronizeAll(ctx, false); err == nil || !strings.Contains(err.Error(), "activated checkout is dirty") {
		t.Fatalf("sync-all error = %v", err)
	}
	if _, err := manager.SynchronizeDefinition(ctx, "acme__orders__table0000", ""); err == nil || !strings.Contains(err.Error(), "activated checkout is dirty") {
		t.Fatalf("single sync error = %v", err)
	}
	if len(runner.calls) != before || len(guard.calls) != 2 {
		t.Fatalf("guard calls=%#v evaluator calls=%d, want no source evaluation", guard.calls, len(runner.calls)-before)
	}

	candidate := t.TempDir()
	if err := os.MkdirAll(filepath.Join(candidate, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "tables", "next.ts"), []byte("export default {};\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := evaluator.Prepare(ctx, []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "candidate"}}); err != nil {
		t.Fatal(err)
	}
	if len(guard.calls) != 2 || len(runner.calls) != before+1 || len(runner.calls[before].Mounts) != 1 || runner.calls[before].Mounts[0].Source != candidate {
		t.Fatalf("candidate guard=%#v call=%#v", guard.calls, runner.calls[before:])
	}
	if err := evaluator.Complete(ctx, true); err != nil {
		t.Fatal(err)
	}
}

func TestCandidatePreparationIsDurableAndRollbackRetiresCandidate(t *testing.T) {
	evaluator, _, manager, _ := testEvaluator(t, 0)
	if err := manager.FinalizeFullSynchronization(context.Background(), nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteInitialization(context.Background(), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	candidate := t.TempDir()
	if err := os.MkdirAll(filepath.Join(candidate, "tables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "tables", "new_table.ts"), []byte("export default {};\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := evaluator.Prepare(context.Background(), []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: "candidate"}}); err != nil {
		t.Fatal(err)
	}
	if !manager.Status().PendingDeployment {
		t.Fatal("prepared schema deployment is not durable")
	}
	detail, err := manager.InspectTable(context.Background(), "acme__orders__new_table")
	if err != nil || detail.State != "active" {
		t.Fatalf("prepared table = %#v, %v", detail, err)
	}
	if err := evaluator.Complete(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	detail, err = manager.InspectTable(context.Background(), "acme__orders__new_table")
	if err != nil || detail.State != "retired" || manager.Status().PendingDeployment {
		t.Fatalf("rolled-back table = %#v, pending=%t, %v", detail, manager.Status().PendingDeployment, err)
	}
}

func TestUninitializedCatalogAllowsPackageRecoveryBeforeFullRetry(t *testing.T) {
	evaluator, runner, manager, packageRoot := testEvaluator(t, 0)
	if manager.Status().Initialized {
		t.Fatal("test catalog unexpectedly initialized")
	}
	if err := evaluator.Prepare(context.Background(), []deployment.Candidate{{
		PackageID: "acme/orders", Root: packageRoot, Commit: "replacement",
	}}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || manager.Status().PendingDeployment {
		t.Fatalf("uninitialized recovery evaluated=%d pending=%t", len(runner.calls), manager.Status().PendingDeployment)
	}
	if err := evaluator.Complete(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationRejectsCanonicalCollisionsAndMalformedResults(t *testing.T) {
	evaluator, runner, _, root := testEvaluator(t, 0)
	for _, name := range []string{"same-name.ts", "same_name.ts"} {
		if err := os.WriteFile(filepath.Join(root, "tables", name), []byte("export default {};\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := evaluator.Evaluate(context.Background(), nil); err == nil {
		t.Fatal("canonical table collision was accepted")
	}
	if err := os.Remove(filepath.Join(root, "tables", "same-name.ts")); err != nil {
		t.Fatal(err)
	}
	runner.malformed = true
	if _, err := evaluator.Evaluate(context.Background(), nil); err == nil {
		t.Fatal("malformed evaluator response was accepted")
	}
}

func TestInitialSynchronizationResumesCompletedBatches(t *testing.T) {
	evaluator, runner, manager, _ := testEvaluator(t, maximumBatch+1)
	runner.failAt = 2
	results, err := evaluator.SynchronizeAll(context.Background(), true)
	if err == nil || len(results) != maximumBatch || manager.Status().State != database.StateInitializationFailed {
		t.Fatalf("first synchronization results=%d status=%#v error=%v", len(results), manager.Status(), err)
	}
	runner.failAt = 0
	before := len(runner.calls)
	results, err = evaluator.SynchronizeAll(context.Background(), true)
	if err != nil || len(results) != 1 || len(runner.calls)-before != 1 || !manager.Status().Initialized {
		t.Fatalf("resumed results=%d calls=%d status=%#v error=%v", len(results), len(runner.calls)-before, manager.Status(), err)
	}
}

func TestPendingDeploymentRecoveryAlignsTheActivePackageTree(t *testing.T) {
	evaluator, runner, manager, packageRoot := testEvaluator(t, 1)
	ctx := context.Background()
	activeCommit := commitRepository(t, packageRoot, "active")
	if _, err := evaluator.SynchronizeAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	tableID := "acme__orders__table0000"
	if _, err := manager.BeginDeployment(ctx, []database.DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "candidate"}}); err != nil {
		t.Fatal(err)
	}
	descriptor := database.TableDescriptor{
		FormatVersion: 1, TableID: tableID,
		Columns:    []database.ColumnDescriptor{{Name: "id", LogicalType: "text", PrimaryKey: true}},
		PrimaryKey: []string{"id"},
		Indexes:    []database.IndexDescriptor{{Name: "candidate_id_index", Columns: []string{"id"}}},
	}
	encoded, _ := json.Marshal(descriptor)
	digest := sha256.Sum256(encoded)
	candidate := database.EvaluatedTable{
		Descriptor: descriptor, DescriptorJSON: string(encoded), DescriptorHash: hex.EncodeToString(digest[:]),
		SourceModule:  packageMountRoot + "/acme/orders/tables/" + tableFile(0),
		SourcePackage: "acme/orders", SourceCommit: "candidate",
		Dependencies: []string{packageMountRoot + "/acme/orders/tables/" + tableFile(0)},
	}
	if _, err := manager.Synchronize(ctx, []database.EvaluatedTable{candidate}, database.SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.RecoverAll(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.InspectTable(ctx, tableID)
	if err != nil || len(detail.PhysicalIndexes) != 0 || detail.SourceCommit != activeCommit || manager.Status().PendingDeployment {
		t.Fatalf("recovered active schema = %#v, pending=%t, %v", detail, manager.Status().PendingDeployment, err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("recovery evaluator calls = %d, want 2", len(runner.calls))
	}
}

func TestIncrementalPreparationEvaluatesOnlyChangedAndDependentTables(t *testing.T) {
	evaluator, runner, manager, packageRoot := testEvaluator(t, 2)
	ctx := context.Background()
	shared := filepath.Join(packageRoot, "src", "shared.ts")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("export const value = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCommit := commitRepository(t, packageRoot, "initial")
	firstModule := packageMountRoot + "/acme/orders/tables/" + tableFile(0)
	runner.dependencies = map[string][]string{
		firstModule: {firstModule, packageMountRoot + "/acme/orders/src/shared.ts"},
	}
	if _, err := evaluator.SynchronizeAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	if manager.Status().PackageSetHash == "" {
		t.Fatal("initial package fingerprint is missing")
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	command := exec.Command("git", "clone", "--quiet", packageRoot, candidate)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone candidate: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(candidate, "src", "shared.ts"), []byte("export const value = 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(candidate, "tables", tableFile(1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "tables", tableFile(2)), []byte("export default {};\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newCommit := commitRepository(t, candidate, "candidate")
	if oldCommit == newCommit {
		t.Fatal("candidate commit did not change")
	}
	before := len(runner.calls)
	if err := evaluator.Prepare(ctx, []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: newCommit}}); err != nil {
		t.Fatal(err)
	}
	if evaluator.releaseDeployment == nil || evaluator.deploymentContext == nil {
		t.Fatal("schema deployment lock was not retained through source activation")
	}
	modules := []string{}
	for _, call := range runner.calls[before:] {
		modules = append(modules, call.CheckModules...)
	}
	sort.Strings(modules)
	wanted := []string{
		packageMountRoot + "/acme/orders/tables/" + tableFile(0),
		packageMountRoot + "/acme/orders/tables/" + tableFile(2),
	}
	if len(modules) != len(wanted) || modules[0] != wanted[0] || modules[1] != wanted[1] {
		t.Fatalf("evaluated modules = %#v, want %#v", modules, wanted)
	}
	retired, err := manager.InspectTable(ctx, "acme__orders__table0001")
	if err != nil || retired.State != "retired" {
		t.Fatalf("deleted definition = %#v, %v", retired, err)
	}
	if err := evaluator.Complete(ctx, false); err != nil {
		t.Fatal(err)
	}
	if evaluator.releaseDeployment != nil || evaluator.deploymentContext != nil {
		t.Fatal("schema deployment lock was not released after completion")
	}
}

func commitRepository(t *testing.T, root, message string) string {
	t.Helper()
	commands := [][]string{
		{"init", "--quiet", "-b", "main"},
		{"config", "user.name", "Database Test"},
		{"config", "user.email", "database@example.test"},
		{"add", "--all"},
		{"commit", "--quiet", "-m", message},
	}
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			if arguments[0] == "init" {
				continue
			}
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output[:len(output)-1])
}
