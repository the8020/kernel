package packages

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"the8020/kernel/database"
)

type countingDatabaseStore struct {
	database.Store
	queries int
}

func (s *countingDatabaseStore) QueryContext(ctx context.Context, statement string, arguments ...any) (*sql.Rows, error) {
	s.queries++
	return s.Store.QueryContext(ctx, statement, arguments...)
}

func (s *countingDatabaseStore) QueryRowContext(ctx context.Context, statement string, arguments ...any) database.Row {
	s.queries++
	return s.Store.QueryRowContext(ctx, statement, arguments...)
}

func packageDatabase(t *testing.T) *database.Manager {
	t.Helper()
	db := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"),
		MaximumOpenConnections: 8, MaximumIdleConnections: 2,
	})
	if _, err := db.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE "the8020__packages__packages" ("packageId" TEXT PRIMARY KEY, "author" TEXT NOT NULL, "repository" TEXT NOT NULL, "source" TEXT, "requestedCommit" TEXT, "requestedTag" TEXT, "secretName" TEXT, "local" INTEGER NOT NULL, "activeCommit" TEXT, "state" TEXT NOT NULL, "error" TEXT, "revision" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__packages__activations" ("activationId" TEXT PRIMARY KEY, "stage" TEXT NOT NULL, "error" TEXT, "previousPackageSetHash" TEXT NOT NULL, "candidatePackageSetHash" TEXT NOT NULL, "startedAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL, "completedAt" TEXT) STRICT`,
		`CREATE TABLE "the8020__packages__activation_packages" ("activationId" TEXT NOT NULL, "packageId" TEXT NOT NULL, "previousCommit" TEXT, "candidateCommit" TEXT NOT NULL, "firstActivation" INTEGER NOT NULL, PRIMARY KEY ("activationId", "packageId")) STRICT`,
		`CREATE TABLE "the8020__packages__hook_runs" ("activationId" TEXT NOT NULL, "packageId" TEXT NOT NULL, "hook" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" INTEGER NOT NULL, "error" TEXT, "startedAt" TEXT, "completedAt" TEXT, PRIMARY KEY ("activationId", "packageId", "hook")) STRICT`,
		`CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE INDEX "the8020__system__revisions__revision__index" ON "the8020__system__revisions" ("revision")`,
		`CREATE TABLE "the8020__services__services" ("serviceId" TEXT PRIMARY KEY, "packageId" TEXT NOT NULL, "packageCommit" TEXT NOT NULL, "manifestHash" TEXT NOT NULL, "description" TEXT NOT NULL, "entrypoint" TEXT NOT NULL, "accessMode" TEXT NOT NULL, "unauthenticatedAction" TEXT NOT NULL, "unauthenticatedStatus" INTEGER NOT NULL, "unauthenticatedMessage" TEXT NOT NULL, "unauthenticatedRedirectUrl" TEXT NOT NULL, "declaredServiceType" TEXT, "declaredSessionKeepAliveMs" INTEGER, "declaredMinimumWorkers" INTEGER, "declaredMaximumWorkers" INTEGER, "declaredConcurrencyPerWorker" INTEGER, "declaredTargetUtilization" REAL, "declaredWorkerKeepAliveMs" INTEGER, "declaredSandboxGroup" TEXT, "declaredMinimumSandboxes" INTEGER, "declaredWorkersPerSandbox" INTEGER, "enabled" INTEGER NOT NULL, "active" INTEGER NOT NULL, "desiredVersion" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__services__overrides" ("serviceId" TEXT PRIMARY KEY, "serviceType" TEXT, "sessionKeepAliveMs" INTEGER, "minimumWorkers" INTEGER, "maximumWorkers" INTEGER, "concurrencyPerWorker" INTEGER, "targetUtilization" REAL, "workerKeepAliveMs" INTEGER, "sandboxGroup" TEXT, "minimumSandboxes" INTEGER, "workersPerSandbox" INTEGER, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__services__versions" ("serviceId" TEXT NOT NULL, "version" INTEGER NOT NULL, "packageCommit" TEXT NOT NULL, "manifestHash" TEXT NOT NULL, "policyHash" TEXT NOT NULL, "serviceType" TEXT NOT NULL, "sessionKeepAliveMs" INTEGER NOT NULL, "minimumWorkers" INTEGER NOT NULL, "maximumWorkers" INTEGER NOT NULL, "concurrencyPerWorker" INTEGER NOT NULL, "targetUtilization" REAL NOT NULL, "workerKeepAliveMs" INTEGER NOT NULL, "sandboxGroup" TEXT NOT NULL, "minimumSandboxes" INTEGER NOT NULL, "workersPerSandbox" INTEGER NOT NULL, "createdAt" TEXT NOT NULL, PRIMARY KEY ("serviceId", "version")) STRICT`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDatabasePackageIndexStoresDesiredAndActiveCommit(t *testing.T) {
	db := packageDatabase(t)
	store, err := NewDatabasePackageIndexStore(db)
	if err != nil {
		t.Fatal(err)
	}
	entry := PackageIndex{Author: "acme", Repository: "orders", PackageID: "acme/orders", Source: "https://example.test/acme/orders.git", Commit: "abcdef1", Secret: "git"}
	if err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Get(context.Background(), entry.PackageID)
	if err != nil || !exists || loaded.Source != entry.Source || loaded.Commit != entry.Commit || loaded.Secret != "git" {
		t.Fatalf("loaded=%#v exists=%t err=%v", loaded, exists, err)
	}
	if err := store.SetActivation(context.Background(), entry.PackageID, "ready", "0123456789", nil); err != nil {
		t.Fatal(err)
	}
	var state, commit string
	if err := db.QueryRowContext(context.Background(), `SELECT "state", "activeCommit" FROM "the8020__packages__packages" WHERE "packageId" = $1`, entry.PackageID).Scan(&state, &commit); err != nil || state != "ready" || commit != "0123456789" {
		t.Fatalf("state=%q commit=%q err=%v", state, commit, err)
	}
}

func TestDatabaseServiceStateSeparatesManifestDefaultsFromOverrides(t *testing.T) {
	db := packageDatabase(t)
	store, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		Identity: Identity{Namespace: "acme", Repository: "orders", Service: "api"},
		Service: ServiceManifest{
			Schema: 2, Description: "Orders API", Entrypoint: "service.ts",
			Lifecycle: LifecycleManifest{DefaultEnabled: true, ServiceType: ServiceTypeStateless},
			Access:    AccessManifest{Mode: AccessModePublic, Unauthenticated: UnauthenticatedManifest{Action: UnauthenticatedReject, Status: 401, Message: "Authentication is required."}},
		},
	}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{WorkersPerSandbox: 4},
	}
	state := DesiredServiceState{Enabled: true}
	if err := store.InstallDefinition(context.Background(), definition, state, effective, "commit-one"); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Get("acme/orders/api")
	if err != nil || !exists || !loaded.Enabled || loaded.Scaling.ConcurrencyPerWorker != nil {
		t.Fatalf("state=%#v exists=%t err=%v", loaded, exists, err)
	}
	minimum := 3
	loaded.Generation = 1
	loaded.Scaling.MinimumWorkers = &minimum
	if err := store.Put("acme/orders/api", loaded); err != nil {
		t.Fatal(err)
	}
	loaded, _, err = store.Get("acme/orders/api")
	if err != nil || loaded.Scaling.MinimumWorkers == nil || *loaded.Scaling.MinimumWorkers != 3 || loaded.Scaling.MaximumWorkers != nil {
		t.Fatalf("operator overrides=%#v err=%v", loaded, err)
	}
	var versions int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM "the8020__services__versions" WHERE "serviceId" = $1`, "acme/orders/api").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("versions=%d err=%v", versions, err)
	}
}

func TestDatabaseServiceStateListUsesOneSnapshotQuery(t *testing.T) {
	db := packageDatabase(t)
	writer, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{WorkersPerSandbox: 4},
	}
	for _, serviceID := range []string{"acme/orders/api", "acme/orders/events"} {
		if err := writer.InstallDefinition(context.Background(), databaseServiceDefinition(t, serviceID), DesiredServiceState{Enabled: true}, effective, "commit-one"); err != nil {
			t.Fatal(err)
		}
	}
	counting := &countingDatabaseStore{Store: db}
	reader, err := NewDatabaseServiceStateStore(counting)
	if err != nil {
		t.Fatal(err)
	}
	items, err := reader.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("services=%#v err=%v", items, err)
	}
	if counting.queries != 1 {
		t.Fatalf("service list used %d database queries, want one", counting.queries)
	}
}

func TestServiceRevisionFollowerObservesOnlyRemoteDesiredStateChanges(t *testing.T) {
	db := packageDatabase(t)
	writer, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{MinimumWorkers: 1, ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{MinimumSandboxes: 1, WorkersPerSandbox: 4},
	}
	for _, serviceID := range []string{"acme/orders/api", "acme/orders/events"} {
		if err := writer.InstallDefinition(context.Background(), databaseServiceDefinition(t, serviceID), DesiredServiceState{}, effective, "commit-one"); err != nil {
			t.Fatal(err)
		}
	}
	// Definition staging is published by the package revision only after
	// activation; it must not wake service followers prematurely.
	if revision, err := writer.ServiceRevision(context.Background()); err != nil || revision != 0 {
		t.Fatalf("staged definition revision=%d err=%v", revision, err)
	}
	follower, err := NewServiceRevisionFollower(context.Background(), &Store{state: reader})
	if err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 {
		t.Fatalf("unchanged poll=%#v err=%v", update, err)
	}

	state := DesiredServiceState{Enabled: true, Generation: 1}
	if err := writer.UpdateDesiredDefinition(context.Background(), databaseServiceDefinition(t, "acme/orders/api"), state, effective, "commit-one"); err != nil {
		t.Fatal(err)
	}
	update, err := follower.Poll(context.Background())
	if err != nil || update.Revision != 1 || !slices.Equal(update.ReconcileServices, []string{"acme/orders/api"}) || len(update.RetireServices) != 0 {
		t.Fatalf("first remote update=%#v err=%v", update, err)
	}
	if retry, err := follower.Poll(context.Background()); err != nil || !slices.Equal(retry.ReconcileServices, update.ReconcileServices) {
		t.Fatalf("unacknowledged update was not retried: %#v err=%v", retry, err)
	}
	if err := follower.Acknowledge(update.Revision); err != nil {
		t.Fatal(err)
	}

	state = DesiredServiceState{Enabled: false, Generation: 1}
	if err := writer.UpdateDesiredDefinition(context.Background(), databaseServiceDefinition(t, "acme/orders/events"), state, effective, "commit-one"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete("acme/orders/api"); err != nil {
		t.Fatal(err)
	}
	update, err = follower.Poll(context.Background())
	if err != nil || update.Revision != 3 || !slices.Equal(update.ReconcileServices, []string{"acme/orders/events"}) || !slices.Equal(update.RetireServices, []string{"acme/orders/api"}) {
		t.Fatalf("batched remote update=%#v err=%v", update, err)
	}
	var markers int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM "the8020__system__revisions" WHERE "domain" LIKE 'service:%'`).Scan(&markers); err != nil || markers != 2 {
		t.Fatalf("latest marker count=%d err=%v", markers, err)
	}
}

func TestDesiredServiceMutationRollsBackWhenRevisionCannotPublish(t *testing.T) {
	db := packageDatabase(t)
	store, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{MinimumWorkers: 1, ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{MinimumSandboxes: 1, WorkersPerSandbox: 4},
	}
	definition := databaseServiceDefinition(t, "acme/orders/api")
	if err := store.InstallDefinition(context.Background(), definition, DesiredServiceState{}, effective, "commit-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TABLE "the8020__system__revisions"`); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateDesiredDefinition(context.Background(), definition, DesiredServiceState{Enabled: true, Generation: 1}, effective, "commit-one"); err == nil {
		t.Fatal("desired state committed without its revision")
	}
	state, exists, err := store.Get("acme/orders/api")
	if err != nil || !exists || state.Enabled || state.Generation != 0 {
		t.Fatalf("partially committed state=%#v exists=%t err=%v", state, exists, err)
	}
	var versions int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM "the8020__services__versions" WHERE "serviceId" = $1`, "acme/orders/api").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("partially committed versions=%d err=%v", versions, err)
	}
}

func TestPackageServiceRetirementIsAtomic(t *testing.T) {
	db := packageDatabase(t)
	store, err := NewDatabaseServiceStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{WorkersPerSandbox: 4},
	}
	for _, serviceID := range []string{"acme/orders/api", "acme/orders/events"} {
		if err := store.InstallDefinition(context.Background(), databaseServiceDefinition(t, serviceID), DesiredServiceState{Enabled: true}, effective, "commit-one"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER "reject_events_retirement" BEFORE UPDATE OF "active" ON `+servicesTable+` WHEN OLD."serviceId" = 'acme/orders/events' AND NEW."active" = 0 BEGIN SELECT RAISE(ABORT, 'forced retirement failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RetirePackage(context.Background(), "acme/orders", nil); err == nil {
		t.Fatal("partial package retirement was accepted")
	}
	var active int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+servicesTable+` WHERE "packageId" = $1 AND "active" = 1 AND "enabled" = 1`, "acme/orders").Scan(&active); err != nil || active != 2 {
		t.Fatalf("package retirement was not rolled back: active=%d err=%v", active, err)
	}
}

func databaseServiceDefinition(t *testing.T, serviceID string) Definition {
	t.Helper()
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return Definition{Identity: identity, Service: ServiceManifest{
		Schema: serviceManifestSchema, Description: serviceID, Entrypoint: "service.ts",
		Lifecycle: LifecycleManifest{DefaultEnabled: true, ServiceType: ServiceTypeStateless},
		Access: AccessManifest{Mode: AccessModePublic, Unauthenticated: UnauthenticatedManifest{
			Action: UnauthenticatedReject, Status: 401, Message: "Authentication is required.",
		}},
	}}
}
