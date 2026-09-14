package database_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	. "the8020/kernel/database"
	"the8020/kernel/database/schematest"
	"time"
)

func evaluatedTable(t *testing.T, descriptor schematest.Descriptor) EvaluatedTable {
	t.Helper()
	encoded, err := canonicalJSON(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encoded)
	return EvaluatedTable{
		Descriptor: schematest.Transport(descriptor), DescriptorJSON: string(encoded), DescriptorHash: hex.EncodeToString(hash[:]),
		SourceModule: "/workspace/packages/acme/orders/tables/orders.ts", SourcePackage: "acme/orders", SourceCommit: strings.Repeat("a", 40),
		Dependencies: []string{"/workspace/packages/acme/orders/tables/orders.ts"},
	}
}

func testDescriptor() schematest.Descriptor {
	return schematest.Descriptor{
		FormatVersion: 1,
		TableID:       "acme__orders__orders",
		Columns: []ColumnDescriptor{
			{Name: "id", LogicalType: "integer", Generated: true, PrimaryKey: true},
			{Name: "status", LogicalType: "enum", EnumValues: []string{"draft", "confirmed"}, Default: &DefaultDescriptor{Kind: "literal", Value: "draft"}},
			{Name: "total", LogicalType: "decimal", Precision: 18, Scale: 2},
			{Name: "metadata", LogicalType: "json", Nullable: true},
		},
		PrimaryKey: []string{"id"},
		Indexes:    []IndexDescriptor{{Name: "orders_status", Columns: []string{"status"}}},
	}
}

func TestActivationOperationOwnership(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	const first, second = "act-aaaaaaaaaa", "act-bbbbbbbbbb"
	owned, release, err := manager.AcquireActivationLock(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, nestedRelease, err := manager.AcquireActivationLock(owned, first); err != nil {
		t.Fatal(err)
	} else {
		nestedRelease()
	}
	if _, duplicateRelease, err := manager.AcquireActivationLock(ctx, first); err == nil {
		duplicateRelease()
		t.Fatal("admitted a duplicate operation or nested release dropped its parent's lock")
	}
	if _, independentRelease, err := manager.AcquireActivationLock(ctx, second); err != nil {
		t.Fatal(err)
	} else {
		independentRelease()
	}
	release()
	_, replacementRelease, err := manager.AcquireActivationLock(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	defer replacementRelease()
	release() // Old duplicate release cannot erase a replacement's ownership.
	if _, duplicateRelease, err := manager.AcquireActivationLock(ctx, first); err == nil {
		duplicateRelease()
		t.Fatal("stale release removed new operation ownership")
	}
	if _, _, err := manager.AcquireActivationLock(ctx, "invalid"); err == nil {
		t.Fatal("accepted invalid activation identity")
	}
}

func TestCatalogBootstrapAndAdditiveSynchronization(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	status, err := manager.InitializeCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateConnected || status.CatalogVersion != 2 || status.Initialized {
		t.Fatalf("catalog status = %#v", status)
	}
	descriptor := testDescriptor()
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{
		Full: true, PackageCommits: map[string]string{"acme/orders": "packages-1"},
	})
	if err != nil || len(results) != 1 || results[0].State != "synchronized" {
		t.Fatalf("synchronize = %#v, %v", results, err)
	}
	if err := manager.CompleteInitialization(ctx, map[string]string{"acme/orders": "packages-1"}); err != nil {
		t.Fatal(err)
	}
	if !manager.Status().Initialized {
		t.Fatal("initial schema synchronization did not initialize catalog")
	}
	detail, err := manager.InspectTable(ctx, descriptor.TableID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.SynchronizationState != "synchronized" || len(detail.Differences) != 0 || len(detail.Physical) != 4 {
		t.Fatalf("table detail = %#v", detail)
	}
	if detail.Columns == nil || detail.PhysicalIndexes == nil || detail.PhysicalChecks == nil || detail.Differences == nil {
		t.Fatalf("table detail contains non-canonical null collections: %#v", detail)
	}
	descriptor.Columns = append(descriptor.Columns, ColumnDescriptor{Name: "note", LogicalType: "text", Nullable: true})
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{RetireMissingPackages: []string{"acme/orders"}}); err != nil {
		t.Fatal(err)
	}
	detail, err = manager.InspectTable(ctx, descriptor.TableID)
	if err != nil || len(detail.Physical) != 5 {
		t.Fatalf("additive detail = %#v, %v", detail, err)
	}
}

func TestDeploymentOutcomeRemainsVisibleAfterRollback(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Synchronize(ctx, nil, SynchronizationOptions{
		Full: true, PackageCommits: map[string]string{"acme/orders": "one"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteInitialization(ctx, map[string]string{"acme/orders": "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginDeployment(ctx, "act-0123456789", []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "two"}}); err != nil {
		t.Fatal(err)
	}
	failure := "candidate table definition is invalid"
	if err := manager.UpdatePendingDeployment(ctx, "act-0123456789", "failed", errors.New(failure)); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteDeployment(ctx, "act-0123456789", false); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.State != StateReady || status.PendingDeployment || status.LastDeploymentAt == "" || status.LastDeploymentError != failure {
		t.Fatalf("rolled-back deployment status = %#v", status)
	}
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if manager.Status().LastDeploymentError != failure {
		t.Fatalf("deployment failure was not durable: %#v", manager.Status())
	}
	if _, err := manager.BeginDeployment(ctx, "act-0123456789", []DeploymentCandidate{{PackageID: "acme/orders", CandidateCommit: "two"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteDeployment(ctx, "act-0123456789", true); err != nil {
		t.Fatal(err)
	}
	status = manager.Status()
	if status.LastDeploymentError != "" || status.PackageSetHash != PackageSetHash(map[string]string{"acme/orders": "two"}) {
		t.Fatalf("successful deployment status = %#v", status)
	}
}

func TestExistingCatalogInitializationDoesNotCompeteWithWriters(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "system.db"))
	manager := newSchemaDatabase(t, config)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	writer, err := manager.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE _8020_catalog SET updated_at = updated_at`); err != nil {
		t.Fatal(err)
	}
	restarted := newSchemaDatabase(t, config)
	t.Cleanup(func() { _ = restarted.Close() })
	validation, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	status, err := restarted.InitializeCatalog(validation)
	if err != nil || status.State != StateConnected {
		t.Fatalf("validate existing catalog alongside a writer: status=%s, error=%v", status.State, err)
	}
}

func TestCatalogBootstrapRejectsAnInvalidExistingCatalog(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.Execute(ctx, `CREATE TABLE _8020_columns (table_id TEXT PRIMARY KEY) STRICT`, nil); err != nil {
		t.Fatal(err)
	}
	status, err := manager.InitializeCatalog(ctx)
	if err == nil || status.State != StateInitializationFailed || !strings.Contains(status.CatalogError, "validate database catalog") {
		t.Fatalf("invalid catalog status = %#v, %v", status, err)
	}
}

func TestUnsafeChangesStopBeforeCatalogSwitch(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{Full: true, PackageCommits: map[string]string{"acme/orders": "one"}}); err != nil {
		t.Fatal(err)
	}
	changed := testDescriptor()
	changed.Columns[2].LogicalType = "float"
	changed.Columns[2].Precision = 0
	changed.Columns[2].Scale = 0
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, changed)}, SynchronizationOptions{RetireMissingPackages: []string{"acme/orders"}})
	if err == nil || results[0].State != "migration_required" {
		t.Fatalf("unsafe synchronization = %#v, %v", results, err)
	}
	detail, inspectErr := manager.InspectTable(ctx, descriptor.TableID)
	if inspectErr != nil || schematest.Decode(detail.Descriptor).Columns[2].LogicalType != "decimal" {
		t.Fatalf("stored descriptor changed after rejected migration: %#v, %v", detail, inspectErr)
	}
}

func TestDescriptorCanonicalJSONMatchesTypeScriptStringEscaping(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	defer manager.Close()
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	descriptor.Columns[3].Default = &DefaultDescriptor{Kind: "literal", Value: map[string]any{"markup": "<strong>&</strong>"}}
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRemovedDefinitionsRetireUntilExplicitTrim(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{Full: true, PackageCommits: map[string]string{"acme/orders": "one"}}); err != nil {
		t.Fatal(err)
	}
	withoutMetadata := testDescriptor()
	withoutMetadata.Columns = withoutMetadata.Columns[:3]
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, withoutMetadata)}, SynchronizationOptions{RetireMissingPackages: []string{"acme/orders"}}); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.InspectTable(ctx, descriptor.TableID)
	if err != nil || detail.RetiredColumns != 1 || len(detail.Physical) != 4 {
		t.Fatalf("retired column detail = %#v, %v", detail, err)
	}
	if err := manager.Trim(ctx, descriptor.TableID, []string{"metadata"}, false); err != nil {
		t.Fatal(err)
	}
	detail, err = manager.InspectTable(ctx, descriptor.TableID)
	if err != nil || len(detail.Physical) != 3 || detail.RetiredColumns != 0 {
		t.Fatalf("trimmed column detail = %#v, %v", detail, err)
	}
	if _, err := manager.Synchronize(ctx, nil, SynchronizationOptions{Full: true, PackageCommits: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	detail, err = manager.InspectTable(ctx, descriptor.TableID)
	if err != nil || detail.State != "retired" {
		t.Fatalf("retired table detail = %#v, %v", detail, err)
	}
	if err := manager.Trim(ctx, descriptor.TableID, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InspectTable(ctx, descriptor.TableID); err == nil {
		t.Fatal("trimmed table remains in catalog")
	}
}

func TestCanonicalTableIDIsPortableAndStable(t *testing.T) {
	id, err := CanonicalTableID("The8020", "Admin_--_Core", "User Sessions")
	if err != nil || id != "the8020__admin_core__user_sessions" {
		t.Fatalf("canonical ID = %q, %v", id, err)
	}
	namespace := strings.Repeat("namespace", 8)
	packageName := strings.Repeat("package", 8)
	tableName := strings.Repeat("table", 8)
	long, err := CanonicalTableID(namespace, packageName, tableName)
	full := strings.Join([]string{namespace, packageName, tableName}, "__")
	digest := sha256.Sum256([]byte(full))
	want := full[:56] + "_" + hex.EncodeToString(digest[:])[:6]
	if err != nil || long != want || len(long) != 63 {
		t.Fatalf("long canonical ID = %q (%d), %v", long, len(long), err)
	}
	descriptor := testDescriptor()
	descriptor.TableID = long
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.InitializeCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Synchronize(context.Background(), []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatalf("synchronize shortened table ID: %v", err)
	}
}

func TestPackageSetHashIsIndependentOfMapOrder(t *testing.T) {
	left := map[string]string{"the8020/demo": "two", "the8020/db": "one"}
	right := map[string]string{}
	right["the8020/db"] = "one"
	right["the8020/demo"] = "two"
	if PackageSetHash(left) != PackageSetHash(right) || PackageSetHash(left) == PackageSetHash(map[string]string{"the8020/db": "different"}) {
		t.Fatal("package-set hash is not deterministic and content-sensitive")
	}
}

func TestCanonicalTableOwnershipCannotBeReplacedByAnotherSource(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	original := evaluatedTable(t, testDescriptor())
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{original}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	conflict := original
	conflict.SourcePackage = "acme/other"
	conflict.SourceModule = "/workspace/packages/acme/other/tables/orders.ts"
	results, err := manager.Synchronize(ctx, []EvaluatedTable{conflict}, SynchronizationOptions{})
	if err == nil || len(results) != 1 || !strings.Contains(results[0].Error, "already owned by acme/orders") {
		t.Fatalf("canonical ownership conflict = %#v, %v", results, err)
	}
}

func TestLogicalReferencesAreValidatedWithoutPhysicalForeignKeys(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	orders := testDescriptor()
	orders.Columns[1].Reference = &ReferenceDescriptor{Table: "acme__customers__customers", Column: "status"}
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, orders)}, SynchronizationOptions{RetireMissingPackages: []string{"acme/orders"}})
	if err == nil || len(results) != 1 || results[0].State != "error" || !strings.Contains(results[0].Error, "references missing column") {
		t.Fatalf("missing logical reference = %#v, %v", results, err)
	}
	customers := schematest.Descriptor{
		FormatVersion: 1, TableID: "acme__customers__customers",
		Columns:    []ColumnDescriptor{{Name: "status", LogicalType: "enum", EnumValues: []string{"draft", "confirmed"}, PrimaryKey: true}},
		PrimaryKey: []string{"status"}, Indexes: []IndexDescriptor{},
	}
	results, err = manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, customers), evaluatedTable(t, orders)}, SynchronizationOptions{})
	if err != nil || len(results) != 2 {
		t.Fatalf("valid logical reference = %#v, %v", results, err)
	}
}

func TestPhysicalAndLogicalCatalogDriftIsDetected(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{Full: true, PackageCommits: map[string]string{"acme/orders": "one"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(ctx, `DROP INDEX "orders_status"`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(ctx, `UPDATE _8020_columns SET logical_type = 'text' WHERE table_id = 'acme__orders__orders' AND column_name = 'total'`, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.InspectTable(ctx, descriptor.TableID)
	if err != nil {
		t.Fatal(err)
	}
	differences := strings.Join(detail.Differences, "\n")
	if detail.SynchronizationState != "drift" || !strings.Contains(differences, "missing index orders_status") || !strings.Contains(differences, "logical catalog column total differs") {
		t.Fatalf("drift detail = %#v", detail)
	}
}

func TestManualMatchingTableIsAdoptedAndUncataloguedTablesRequireExplicitInspection(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	// Create the matching physical table independently of catalog synchronization.
	if _, err := manager.Execute(ctx, `CREATE TABLE acme__orders__orders (
 id INTEGER PRIMARY KEY CONSTRAINT "acme__orders__orders__id__check" CHECK (id BETWEEN -9007199254740991 AND 9007199254740991),
 status TEXT NOT NULL DEFAULT 'draft' CONSTRAINT "acme__orders__orders__status__check" CHECK (status IN ('draft', 'confirmed')),
 total INTEGER NOT NULL CONSTRAINT "acme__orders__orders__total__check" CHECK (total BETWEEN -999999999999999999 AND 999999999999999999),
 metadata TEXT) STRICT`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(ctx, `CREATE TABLE manual_notes (id INTEGER PRIMARY KEY, body TEXT) STRICT`, nil); err != nil {
		t.Fatal(err)
	}
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{})
	if err != nil || len(results) != 1 || results[0].State != "synchronized" {
		t.Fatalf("adoption = %#v, %v", results, err)
	}
	tables, err := manager.ListTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0].TableID != descriptor.TableID {
		t.Fatalf("catalog-only list = %#v", tables)
	}
	detail, err := manager.InspectTable(ctx, "manual_notes")
	if err != nil || len(detail.Physical) != 2 || detail.State != "uncatalogued" {
		t.Fatalf("uncatalogued detail = %#v, %v", detail, err)
	}
	if detail.Columns == nil || detail.PhysicalIndexes == nil || detail.PhysicalChecks == nil || detail.Differences == nil {
		t.Fatalf("uncatalogued detail contains non-canonical null collections: %#v", detail)
	}
}

func TestLogicalReferenceChangeDoesNotRequirePhysicalMigration(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	target := schematest.Descriptor{
		FormatVersion: 1, TableID: "acme__orders__targets",
		Columns: []ColumnDescriptor{
			{Name: "first", LogicalType: "enum", EnumValues: []string{"draft", "confirmed"}, PrimaryKey: true},
			{Name: "second", LogicalType: "enum", EnumValues: []string{"draft", "confirmed"}, Unique: true},
		},
		PrimaryKey: []string{"first"},
		Indexes:    []IndexDescriptor{{Name: "targets_second", Columns: []string{"second"}, Unique: true}},
	}
	orders := testDescriptor()
	orders.Columns[1].Reference = &ReferenceDescriptor{Table: target.TableID, Column: "first"}
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, target), evaluatedTable(t, orders)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	orders.Columns[1].Reference = &ReferenceDescriptor{Table: target.TableID, Column: "second"}
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, orders)}, SynchronizationOptions{})
	if err != nil || results[0].State != "synchronized" {
		t.Fatalf("logical-only change = %#v, %v", results, err)
	}
}

func TestUnsafeAdditionsAndRequiredSQLiteRetirementNeedMigration(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	withNow := testDescriptor()
	withNow.Columns = append(withNow.Columns, ColumnDescriptor{
		Name: "processedAt", LogicalType: "datetime", Default: &DefaultDescriptor{Kind: "now"},
	})
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, withNow)}, SynchronizationOptions{})
	if err == nil || results[0].State != "migration_required" {
		t.Fatalf("defaultNow addition = %#v, %v", results, err)
	}
	withoutTotal := testDescriptor()
	withoutTotal.Columns = append(withoutTotal.Columns[:2], withoutTotal.Columns[3:]...)
	results, err = manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, withoutTotal)}, SynchronizationOptions{})
	if err == nil || results[0].State != "migration_required" {
		t.Fatalf("required retirement = %#v, %v", results, err)
	}
}

func TestRetiredIndexedColumnRemainsPhysicallyCompatible(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	descriptor.Columns = append(descriptor.Columns, ColumnDescriptor{Name: "legacy", LogicalType: "text", Nullable: true})
	descriptor.Indexes = append(descriptor.Indexes, IndexDescriptor{Name: "orders_legacy", Columns: []string{"legacy"}})
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	next := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, next)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.InspectTable(ctx, next.TableID)
	if err != nil || detail.RetiredColumns != 1 || len(detail.Differences) != 0 {
		t.Fatalf("retired indexed column = %#v, %v", detail, err)
	}
	if err := manager.Trim(ctx, next.TableID, []string{"legacy"}, false); err != nil {
		t.Fatal(err)
	}
	detail, err = manager.InspectTable(ctx, next.TableID)
	if err != nil || detail.RetiredColumns != 0 || len(detail.PhysicalIndexes) != len(next.Indexes) || len(detail.Differences) != 0 {
		t.Fatalf("trimmed indexed column = %#v, %v", detail, err)
	}
}

func TestRecoveryRemovesCandidateIndexOnActiveColumns(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	previous := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, previous)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	candidate := testDescriptor()
	candidate.Indexes = append(candidate.Indexes, IndexDescriptor{Name: "orders_total", Columns: []string{"total"}})
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, candidate)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	results, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, previous)}, SynchronizationOptions{})
	if err == nil || results[0].State != "migration_required" {
		t.Fatalf("ordinary index removal = %#v, %v", results, err)
	}
	results, err = manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, previous)}, SynchronizationOptions{Recovery: true})
	if err != nil || results[0].State != "synchronized" {
		t.Fatalf("recovered index = %#v, %v", results, err)
	}
	detail, err := manager.InspectTable(ctx, previous.TableID)
	if err != nil || len(detail.PhysicalIndexes) != len(previous.Indexes) || len(detail.Differences) != 0 {
		t.Fatalf("recovered detail = %#v, %v", detail, err)
	}
}

func TestSynchronizeDefinitionEvaluatesOnlyTheSelectedTable(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	calls := []TableSource{}
	manager.SetSourceEvaluator(func(_ context.Context, source TableSource) (*EvaluatedTable, error) {
		calls = append(calls, source)
		table := evaluatedTable(t, descriptor)
		return &table, nil
	})
	result, err := manager.SynchronizeDefinition(ctx, descriptor.TableID, "acme/orders")
	if err != nil || result.State != "synchronized" || len(calls) != 1 || calls[0].SourceModule != "" {
		t.Fatalf("new definition sync = %#v, calls=%#v, %v", result, calls, err)
	}
	descriptor.Columns = append(descriptor.Columns, ColumnDescriptor{Name: "note", LogicalType: "text", Nullable: true})
	result, err = manager.SynchronizeDefinition(ctx, descriptor.TableID, "")
	if err != nil || result.State != "synchronized" || len(calls) != 2 || calls[1].SourceModule == "" {
		t.Fatalf("existing definition sync = %#v, calls=%#v, %v", result, calls, err)
	}
}

func TestTableBrowsingAvoidsSourceEvaluationAndComparisonIsExplicit(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor()
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	evaluations := 0
	manager.SetSourceEvaluator(func(_ context.Context, source TableSource) (*EvaluatedTable, error) {
		evaluations++
		table := evaluatedTable(t, descriptor)
		table.SourceCommit = "different"
		return &table, nil
	})
	if _, err := manager.Execute(ctx, `DROP TABLE "acme__orders__orders"`, nil); err != nil {
		t.Fatal(err)
	}
	tables, err := manager.ListTables(ctx)
	if err != nil || len(tables) != 1 {
		t.Fatalf("list = %#v, %v", tables, err)
	}
	if evaluations != 0 || tables[0].SynchronizationState != "synchronized" || tables[0].Error != "" {
		t.Fatalf("catalog-only list = %#v", tables[0])
	}
	detail, err := manager.InspectTable(ctx, descriptor.TableID)
	if err != nil || evaluations != 0 || detail.DefinitionState != "" {
		t.Fatalf("fast detail = %#v evaluations=%d error=%v", detail, evaluations, err)
	}
	compared, err := manager.CompareTable(ctx, descriptor.TableID)
	if err != nil || evaluations != 1 || compared.DefinitionState != "commit_mismatch" || compared.CurrentSourceCommit != "different" {
		t.Fatalf("comparison = %#v evaluations=%d error=%v", compared, evaluations, err)
	}
}

func TestCatalogColumnsPreserveAuthoredOrderAfterRetirement(t *testing.T) {
	manager := newSchemaDatabase(t, sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	descriptor := schematest.Descriptor{
		FormatVersion: 1,
		TableID:       "acme__orders__ordered",
		Columns: []ColumnDescriptor{
			{Name: "id", LogicalType: "text", PrimaryKey: true},
			{Name: "zebra", LogicalType: "text", Nullable: true},
			{Name: "alpha", LogicalType: "text", Nullable: true},
			{Name: "middle", LogicalType: "text", Nullable: true},
		},
		PrimaryKey: []string{"id"},
	}
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	descriptor.Columns = append(descriptor.Columns[:1], descriptor.Columns[2:]...)
	if _, err := manager.Synchronize(ctx, []EvaluatedTable{evaluatedTable(t, descriptor)}, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	detail, err := manager.InspectTable(ctx, descriptor.TableID)
	if err != nil {
		t.Fatal(err)
	}
	wanted := []string{"id", "zebra", "alpha", "middle"}
	wantedOrdinals := []int{0, 1, 1, 2}
	if len(detail.Columns) != len(wanted) {
		t.Fatalf("columns = %#v", detail.Columns)
	}
	for index, name := range wanted {
		if detail.Columns[index].ColumnName != name {
			t.Fatalf("column order = %#v", detail.Columns)
		}
		if detail.Columns[index].Ordinal != wantedOrdinals[index] {
			t.Fatalf("column ordinal = %#v", detail.Columns[index])
		}
	}
	if detail.Columns[1].State != "retired" {
		t.Fatalf("retired column = %#v", detail.Columns[1])
	}
}

func sqliteConfig(location string) Config {
	return Config{Backend: BackendSQLite, Location: location, MaximumOpenConnections: 32, MaximumIdleConnections: 8}
}
func newSchemaDatabase(t *testing.T, config Config) *Manager {
	t.Helper()
	manager := New(config)
	schematest.Attach(t, manager)
	return manager
}
func canonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

type ColumnDescriptor = schematest.ColumnDescriptor
type IndexDescriptor = schematest.IndexDescriptor
type DefaultDescriptor = schematest.DefaultDescriptor
type ReferenceDescriptor = schematest.ReferenceDescriptor

func TestSchemaFailurePreservesNativeSQLAndOpaqueMetadata(t *testing.T) {
	manager := New(sqliteConfig(filepath.Join(t.TempDir(), "system.db")))
	defer manager.Close()
	ctx := context.Background()
	manager.SetSchemaExecutor(func(context.Context, SchemaRequest) (json.RawMessage, error) {
		return nil, errors.New("db package is broken")
	})
	if _, err := manager.InitializeCatalog(ctx); err == nil {
		t.Fatal("accepted failed schema bootstrap")
	}
	token, err := manager.BeginTransaction(ctx, "repair", TransactionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.RunStatement(ctx, "repair", StatementRequest{Statement: "SELECT 1", ReturnRows: true, Transaction: token})
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("native repair SQL unavailable: %#v %v", result, err)
	}
	if err := manager.FinishTransaction(ctx, "repair", token, true); err != nil {
		t.Fatal(err)
	}

	encoded := []byte(`{"table_id":"acme__future__values","columns":[{"name":"quantity","logical_type":"future_type","custom_storage":{"unit":"litre"}}],"new_schema_feature":true}`)
	var descriptor TableDescriptor
	if err := json.Unmarshal(encoded, &descriptor); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := json.Marshal(descriptor)
	if err != nil || !bytes.Equal(roundTrip, encoded) {
		t.Fatalf("kernel changed package-owned descriptor: %s: %v", roundTrip, err)
	}
}

func TestSchemaCatalogAndPhysicalReadsRespectNativeRowLimits(t *testing.T) {
	config := sqliteConfig(filepath.Join(t.TempDir(), "system.db"))
	config.MaximumResultRows = 2
	manager := newSchemaDatabase(t, config)
	t.Cleanup(func() { _ = manager.Close() })
	ctx := context.Background()
	if _, err := manager.InitializeCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	var tables []EvaluatedTable
	for _, name := range []string{"first", "second", "third"} {
		descriptor := testDescriptor()
		descriptor.TableID = "acme__orders__" + name
		descriptor.Indexes[0].Name = name + "_status"
		table := evaluatedTable(t, descriptor)
		table.SourceModule = "/workspace/packages/acme/orders/tables/" + name + ".ts"
		tables = append(tables, table)
	}
	if _, err := manager.Synchronize(ctx, tables, SynchronizationOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompleteInitialization(ctx, map[string]string{"acme/orders": tables[0].SourceCommit}); err != nil {
		t.Fatal(err)
	}
	if listed, err := manager.ListTables(ctx); err != nil || len(listed) != 3 {
		t.Fatalf("catalog scan: %d tables, %v", len(listed), err)
	}
	if detail, err := manager.InspectTable(ctx, tables[0].Descriptor.TableID); err != nil || len(detail.Physical) != 4 || len(detail.Differences) != 0 {
		t.Fatalf("physical scan: %+v, %v", detail, err)
	}
	if err := manager.ValidateCatalogReferences(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := manager.Synchronize(ctx, tables[:1], SynchronizationOptions{RetireTables: []string{"acme__orders__missing"}})
	if err == nil || len(results) != 1 || results[0].State != "synchronized" {
		t.Fatalf("retirement failure lost completed table results: %+v, %v", results, err)
	}
}
