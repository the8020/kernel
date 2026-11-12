package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/database"
	"the8020/kernel/execution"
	"the8020/kernel/execution/coordinator"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/supervisor"
	"the8020/kernel/execution/workers"
	"the8020/kernel/sandbox/manager"
	"the8020/kernel/sandbox/model"
	"the8020/kernel/settings"
	settingsdb "the8020/kernel/settings/dbstore"
)

type jobDefaultsRuntime struct{}

func (jobDefaultsRuntime) Ensure(context.Context, coordinator.Request) (manager.Inspection, error) {
	return manager.Inspection{Spec: model.SandboxSpec{SandboxID: "sbx-0123456789", WorkloadType: model.WorkloadJob}}, nil
}
func (jobDefaultsRuntime) Release(context.Context, string, string, string) error { return nil }
func (jobDefaultsRuntime) Start(_ context.Context, sandbox string, req supervisor.StartWorkerRequest) (workers.Record, error) {
	return workers.Record{SandboxID: sandbox, Worker: supervisor.WorkerStatus{WorkerID: req.Metadata.WorkerID}}, nil
}
func (jobDefaultsRuntime) List(context.Context, string) ([]workers.Record, error) { return nil, nil }
func (jobDefaultsRuntime) RunJob(context.Context, string, []any, map[string]string, []string) (supervisor.JobResult, error) {
	return supervisor.JobResult{}, nil
}
func (jobDefaultsRuntime) StopInSandbox(context.Context, string, string, bool) error { return nil }

func TestPersistedJobDefaultsAttachToBootstrapManager(t *testing.T) {
	ctx := context.Background()
	defs := []settings.Definition{
		{Key: "job.default.maximum_parallel_workers", Type: settings.TypeInteger, Default: int64(4), Environment: "THE8020_JOB_DEFAULT_MAXIMUM_PARALLEL_WORKERS"},
		{Key: "job.default.queued_execution_limit", Type: settings.TypeInteger, Default: int64(1024), Environment: "THE8020_JOB_DEFAULT_QUEUED_EXECUTION_LIMIT"},
		{Key: "job.default.execution_timeout", Type: settings.TypeInteger, Default: int64(300000), Environment: "THE8020_JOB_DEFAULT_EXECUTION_TIMEOUT"},
		{Key: "job.default.idle_runtime_timeout", Type: settings.TypeInteger, Default: int64(60000), Environment: "THE8020_JOB_DEFAULT_IDLE_RUNTIME_TIMEOUT"},
		{Key: "job.default.worker_reuse", Type: settings.TypeBoolean, Default: false, Environment: "THE8020_JOB_DEFAULT_WORKER_REUSE"},
	}
	for i := range defs {
		defs[i].Storage = settings.StorageGlobal
		defs[i].RestartRequired = true
		defs[i].Description = "Job default."
	}
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"), MaximumOpenConnections: 8, MaximumIdleConnections: 2})
	if _, err := db.Check(ctx); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE "the8020__system__settings" ("key" TEXT PRIMARY KEY, "value" TEXT NOT NULL, "definitionHash" TEXT NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
		`CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	store, err := settingsdb.New(db)
	if err != nil {
		t.Fatal(err)
	}
	freshSettings, err := settings.New(defs, settings.PersistencePaths{Node: filepath.Join(t.TempDir(), "kernel.toml")}, nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	profile := model.RuntimeProfile{WorkloadType: model.WorkloadJob, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DependencyMode: model.DependencyCachedOnly, Permissions: model.Permissions{ReadPaths: []string{"/programs"}}, NetworkMode: "netstack", ResourceClass: "job"}
	freshPolicy := jobDefaults(freshSettings)
	freshPolicy.Profile = profile
	freshJobs, err := jobs.New(jobDefaultsRuntime{}, jobDefaultsRuntime{}, freshPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer freshJobs.Close()
	if err := attachGlobalJobDefaults(ctx, freshSettings, nil, freshJobs); err == nil {
		t.Fatal("failed attachment succeeded")
	}
	if record, err := freshJobs.Run(ctx, "recovery", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()}); err != nil || record.State != "SUCCEEDED" || record.Parallelism != 4 {
		t.Fatalf("failed attachment changed bootstrap manager: %#v, %v", record, err)
	}
	if err := attachGlobalJobDefaults(ctx, freshSettings, store, freshJobs); err != nil {
		t.Fatal(err)
	}
	fresh, err := freshJobs.Run(ctx, "fresh", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || fresh.State != "SUCCEEDED" || fresh.Parallelism != 4 || fresh.Timeout != 5*time.Minute {
		t.Fatalf("fresh=%#v err=%v", fresh, err)
	}
	values := []any{int64(2), int64(3), int64(42000), int64(900000), true}
	for i, def := range defs {
		if _, err := store.Set(ctx, def, values[i]); err != nil {
			t.Fatal(err)
		}
	}
	settingsManager, err := settings.New(defs, settings.PersistencePaths{Node: filepath.Join(t.TempDir(), "kernel.toml")}, map[string]string{"job.default.maximum_parallel_workers": "7", "job.default.worker_reuse": "false"}, func(key string) (string, bool) {
		if key == "THE8020_JOB_DEFAULT_MAXIMUM_PARALLEL_WORKERS" {
			return "8", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := jobDefaults(settingsManager)
	policy.Profile = profile
	jobManager, err := jobs.New(jobDefaultsRuntime{}, jobDefaultsRuntime{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer jobManager.Close()
	bootstrap, err := jobManager.Run(ctx, "bootstrap", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || bootstrap.State != "SUCCEEDED" || bootstrap.Parallelism != 7 {
		t.Fatalf("bootstrap=%#v err=%v", bootstrap, err)
	}
	if err := attachGlobalJobDefaults(ctx, settingsManager, store, jobManager); err != nil {
		t.Fatal(err)
	}
	for i, def := range defs {
		info, err := settingsManager.Get(def.Key)
		if err != nil || info.ActiveValue != values[i] || info.Source != "persisted" || info.RestartPending {
			t.Fatalf("%s = %#v, %v", def.Key, info, err)
		}
	}
	record, err := jobManager.Run(ctx, "job", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || record.State != "IDLE" || record.Parallelism != 2 || record.Timeout != 42*time.Second {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	noReuse := false
	override, err := jobManager.Run(ctx, "override", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser(), Reuse: &noReuse, Parallelism: 1, Timeout: time.Second})
	if err != nil || override.State != "SUCCEEDED" || override.Parallelism != 1 || override.Timeout != time.Second {
		t.Fatalf("override=%#v err=%v", override, err)
	}
	updated, err := settingsManager.Set(ctx, "job.default.maximum_parallel_workers", "3")
	if err != nil || !updated.RestartPending || updated.ActiveValue != int64(2) {
		t.Fatalf("restart setting=%#v err=%v", updated, err)
	}
	record, err = jobManager.Run(ctx, "after-change", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || record.Parallelism != 2 {
		t.Fatalf("restart setting applied early: %#v err=%v", record, err)
	}
}
