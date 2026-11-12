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

type jobDefaultsRuntime struct {
	gate    <-chan struct{}
	started chan<- struct{}
}

func (jobDefaultsRuntime) Ensure(context.Context, coordinator.Request) (manager.Inspection, error) {
	return manager.Inspection{Spec: model.SandboxSpec{SandboxID: "sbx-0123456789", WorkloadType: model.WorkloadJob}}, nil
}
func (jobDefaultsRuntime) Release(context.Context, string, string, string) error { return nil }
func (jobDefaultsRuntime) Start(_ context.Context, sandbox string, req supervisor.StartWorkerRequest) (workers.Record, error) {
	return workers.Record{SandboxID: sandbox, Worker: supervisor.WorkerStatus{WorkerID: req.Metadata.WorkerID}}, nil
}
func (jobDefaultsRuntime) List(context.Context, string) ([]workers.Record, error) { return nil, nil }
func (r jobDefaultsRuntime) RunJob(ctx context.Context, _ string, args []any, _ map[string]string, _ []string) (supervisor.JobResult, error) {
	if len(args) > 0 && args[0] == "block" {
		r.started <- struct{}{}
		select {
		case <-r.gate:
		case <-ctx.Done():
			return supervisor.JobResult{}, ctx.Err()
		}
	}
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
		defs[i].RuntimeMutable = true
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
	values := []any{int64(2), int64(3), int64(42000), int64(100), true}
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
	gate := make(chan struct{})
	started := make(chan struct{}, 5)
	runtime := jobDefaultsRuntime{gate: gate, started: started}
	jobManager, err := jobs.New(runtime, runtime, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer jobManager.Close()
	defer func() {
		if gate != nil {
			close(gate)
		}
	}()
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
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := jobManager.Inspect(record.ExecutionID); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("persisted idle timeout did not retire the worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	options := jobs.Options{User: execution.SystemUser(), Detached: true, Arguments: []any{"block"}}
	for i := 0; i < 2; i++ {
		if _, err := jobManager.Run(ctx, "running", "file:///programs/job.ts", options); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("job did not start")
		}
	}
	for i := 0; i < 3; i++ {
		queued, err := jobManager.Run(ctx, "queued", "file:///programs/job.ts", options)
		if err != nil || queued.State != "QUEUED" {
			t.Fatalf("queued=%#v err=%v", queued, err)
		}
	}
	if _, err := jobManager.Run(ctx, "overflow", "file:///programs/job.ts", options); err == nil || err.Error() != "job queue limit 3 reached" {
		t.Fatalf("persisted queue limit: %v", err)
	}
	close(gate)
	gate = nil
	deadline = time.Now().Add(3 * time.Second)
	for {
		records, err := jobManager.List()
		if err != nil {
			t.Fatal(err)
		}
		busy := false
		for _, item := range records {
			if item.State == "QUEUED" || item.State == "STARTING" || item.State == "RUNNING" {
				busy = true
			}
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("jobs did not drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	noReuse := false
	override, err := jobManager.Run(ctx, "override", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser(), Reuse: &noReuse, Parallelism: 1, Timeout: time.Second})
	if err != nil || override.State != "SUCCEEDED" || override.Parallelism != 1 || override.Timeout != time.Second {
		t.Fatalf("override=%#v err=%v", override, err)
	}
	updated, err := settingsManager.Set(ctx, "job.default.maximum_parallel_workers", "3")
	if err != nil || updated.RestartPending || updated.ActiveValue != int64(3) {
		t.Fatalf("restart setting=%#v err=%v", updated, err)
	}
	record, err = jobManager.Run(ctx, "after-change", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || record.Parallelism != 3 {
		t.Fatalf("live setting not applied: %#v err=%v", record, err)
	}
	for key, value := range map[string]string{
		"job.default.queued_execution_limit": "2", "job.default.execution_timeout": "21000",
		"job.default.worker_reuse": "false", "job.default.idle_runtime_timeout": "1000",
	} {
		if info, err := settingsManager.Set(ctx, key, value); err != nil || info.RestartPending || info.ConfiguredValue != info.ActiveValue {
			t.Fatalf("live %s: %#v, %v", key, info, err)
		}
	}
	record, err = jobManager.Run(ctx, "live", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || record.State != "SUCCEEDED" || record.Timeout != 21*time.Second {
		t.Fatalf("live timeout/reuse: %#v, %v", record, err)
	}
	// This node has missed the other node's writes; its own write must not hide them.
	if _, err := freshSettings.Set(ctx, "job.default.worker_reuse", "true"); err != nil {
		t.Fatal(err)
	}
	if changed, err := freshSettings.RefreshGlobal(ctx); err != nil || !changed {
		t.Fatalf("cross-node refresh: %v, %v", changed, err)
	}
	record, err = freshJobs.Run(ctx, "follower", "file:///programs/job.ts", jobs.Options{User: execution.SystemUser()})
	if err != nil || record.Parallelism != 3 || record.Timeout != 21*time.Second || record.State != "IDLE" {
		t.Fatalf("cross-node job defaults: %#v, %v", record, err)
	}
}
