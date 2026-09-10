package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/discovery"
	"the8020/kernel/database"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/packages"
	"the8020/kernel/webservices"
)

type handlerIndexerStub struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (s *handlerIndexerStub) ReindexHandlers(_ context.Context, ids ...string) (packages.HandlerReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string(nil), ids...))
	return packages.HandlerReport{Events: 3, Hooks: 2}, s.err
}

type commandIndexerStub struct {
	mu    sync.Mutex
	calls [][]string
}

func (s *commandIndexerStub) Reindex(_ context.Context, ids ...string) (discovery.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string(nil), ids...))
	return discovery.Report{Revision: "new", Commands: 7}, nil
}

func TestReindexRefreshesHandlersAndCommandsWithTheSameSelection(t *testing.T) {
	handlers, commands := &handlerIndexerStub{}, &commandIndexerStub{}
	indexer := &runtimeIndexer{handlers: handlers, commands: commands}
	for _, selection := range [][]string{nil, {"acme/one", "acme/two"}} {
		result, err := indexer.Reindex(context.Background(), selection)
		if err != nil || result["events"] != 3 || result["hooks"] != 2 || result["commands"] != 7 || result["revision"] != "new" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	}
	want := [][]string{nil, {"acme/one", "acme/two"}}
	if !reflect.DeepEqual(handlers.calls, want) || !reflect.DeepEqual(commands.calls, want) {
		t.Fatalf("handlers=%#v commands=%#v", handlers.calls, commands.calls)
	}
	handlers.err = errors.New("invalid hook")
	if _, err := indexer.Reindex(context.Background(), nil); err == nil || len(commands.calls) != 2 {
		t.Fatal("commands refreshed after handler validation failed")
	}
}

func TestPackageConvergenceKeepsNativeIndexFailurePending(t *testing.T) {
	updates := &packageRevisionFollowerStub{update: packages.PackageSetUpdate{Revision: 12, Packages: []string{"acme/changed", "acme/removed"}}}
	calls := 0
	fail := true
	shared := &runtimeSharedState{packages: updates, reindex: func(_ context.Context, ids []string) (core.Result, error) {
		calls++
		if !reflect.DeepEqual(ids, []string{"acme/changed", "acme/removed"}) {
			t.Fatalf("selection=%v", ids)
		}
		if fail {
			return nil, errors.New("invalid native event declaration")
		}
		return core.Result{}, nil
	}}
	if err := shared.Refresh(context.Background()); err == nil || len(updates.acks) != 0 {
		t.Fatal("native failure was acknowledged")
	}
	fail = false
	if err := shared.Refresh(context.Background()); err != nil || calls != 2 || len(updates.acks) != 1 {
		t.Fatalf("calls=%d acks=%v error=%v", calls, updates.acks, err)
	}
	if err := shared.Refresh(context.Background()); err != nil || calls != 2 {
		t.Fatal("unchanged revision repeated discovery")
	}
}

type runtimePackageIndex struct {
	packages.PackageIndexStore
	entries map[string]packages.PackageIndex
}

func (s *runtimePackageIndex) List(context.Context) ([]packages.PackageIndex, error) {
	entries := []packages.PackageIndex{}
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	return entries, nil
}
func (s *runtimePackageIndex) Get(_ context.Context, id string) (packages.PackageIndex, bool, error) {
	entry, ok := s.entries[id]
	return entry, ok, nil
}
func (*runtimePackageIndex) Revision(context.Context) (uint64, error) { return 1, nil }

type indexJobFunc func(context.Context, string, string, jobs.Options) (jobs.Record, error)

func (f indexJobFunc) Run(ctx context.Context, id, entry string, options jobs.Options) (jobs.Record, error) {
	return f(ctx, id, entry, options)
}

func TestIndexingDoesNotWaitForAnotherProvider(t *testing.T) {
	for _, scenario := range []string{"unrelated", "newer-request", "shared-revision"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			root := t.TempDir()
			source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{}}
			for _, id := range []string{"acme/one", "acme/two"} {
				source.entries[id] = packages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
			}
			store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
			if err != nil {
				t.Fatal(err)
			}
			started, waiting := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(waiting) })
			defer release()
			secondStarted := make(chan struct{})
			startedSecond := sync.OnceFunc(func() { close(secondStarted) })
			var first atomic.Bool
			var configuration atomic.Value
			configuration.Store("")
			runner := indexJobFunc(func(ctx context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
				failure := configuration.Load().(string)
				if first.CompareAndSwap(false, true) {
					close(started)
					select {
					case <-waiting:
					case <-ctx.Done():
						return jobs.Record{}, ctx.Err()
					}
				}
				fragments := map[string]any{}
				for _, selected := range options.Arguments[1].(serviceIndexScope).Packages {
					if selected.PackageID == "acme/two" {
						startedSecond()
					}
					fragment := map[string]any{"services": []any{}}
					if failure != "" {
						fragment["error"] = failure
					}
					fragments[selected.PackageID] = fragment
				}
				return jobs.Record{State: "SUCCEEDED", Result: map[string]any{"packages": fragments}}, nil
			})
			indexer := &runtimeIndexer{handlers: &handlerIndexerStub{}, commands: &commandIndexerStub{}, packages: store, jobs: runner, services: webservices.NewIndex()}
			invoke := func(ids []string) error { _, err := indexer.Reindex(ctx, ids); return err }
			var publish func(string, int)
			if scenario == "shared-revision" {
				db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(root, "revision.db"), MaximumOpenConnections: 4})
				t.Cleanup(func() { _ = db.Close() })
				if _, err := db.Check(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
					t.Fatal(err)
				}
				follower, err := packages.NewIndexRevisionFollower(ctx, db)
				if err != nil {
					t.Fatal(err)
				}
				shared := &runtimeSharedState{indexes: follower, reindex: indexer.Reindex, retry: indexer.RetryPending}
				invoke = func([]string) error { return shared.Refresh(ctx) }
				publish = func(id string, revision int) {
					if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('indexes', $1, ''), ($2, $1, '') ON CONFLICT ("domain") DO UPDATE SET "revision" = excluded."revision"`, revision, "index:"+id); err != nil {
						t.Fatal(err)
					}
				}
				publish("acme/one", 1)
			}
			one, two := make(chan error, 1), make(chan error, 1)
			go func() { one <- invoke([]string{"acme/one"}) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first provider did not start")
			}
			selected := "acme/two"
			if scenario == "newer-request" {
				selected = "acme/one"
				configuration.Store("updated provider configuration rejected")
			}
			if publish != nil {
				publish("acme/two", 2)
			}
			go func() {
				if scenario == "newer-request" {
					_, err := indexer.Queue(ctx, []string{selected})
					two <- err
				} else {
					two <- invoke([]string{selected})
				}
			}()
			if scenario == "shared-revision" {
				// This revision includes both packages. Apply its available owner
				// before waiting to finish the selected busy owner.
				select {
				case <-secondStarted:
				case <-time.After(time.Second):
					t.Fatal("new revision did not advance its unrelated package")
				}
				release()
			}
			blocked := false
			select {
			case err := <-two:
				var pending *indexPublicationError
				if scenario != "newer-request" && err != nil || scenario == "newer-request" && !errors.As(err, &pending) {
					t.Errorf("second indexing result: %v", err)
				}
			case <-time.After(time.Second):
				blocked = true
			}
			release()
			if err := <-one; err != nil {
				t.Fatalf("first indexing failed: %v", err)
			}
			if blocked {
				<-two
				t.Fatal("indexing waited behind another provider job")
			}
			if scenario == "newer-request" {
				if err := indexer.RetryPending(ctx); err == nil || !strings.Contains(err.Error(), "updated provider configuration rejected") {
					t.Fatalf("older provider result lost the newer refresh request: %v", err)
				}
			}
		})
	}
}

func TestExplicitIndexingWaitsForSelectedProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root := t.TempDir()
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{}}
	for _, id := range []string{"acme/one", "acme/two"} {
		source.entries[id] = packages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
	}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	started, waiting := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(waiting) })
	var calls, configuration, observed atomic.Int32
	configuration.Store(1)
	indexer := &runtimeIndexer{handlers: &handlerIndexerStub{}, commands: &commandIndexerStub{}, packages: store, services: webservices.NewIndex(), jobs: indexJobFunc(func(ctx context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		for _, selected := range options.Arguments[1].(serviceIndexScope).Packages {
			if selected.PackageID != "acme/one" {
				continue
			}
			version := configuration.Load()
			if calls.Add(1) == 1 {
				close(started)
				select {
				case <-waiting:
				case <-ctx.Done():
					return jobs.Record{}, ctx.Err()
				}
			}
			observed.Store(version)
		}
		return jobs.Record{State: "SUCCEEDED", Result: options.Arguments[2]}, nil
	})}
	indexer.background, indexer.cancel = context.WithCancel(ctx)
	defer func() { release(); _ = indexer.Close() }()
	if _, err := indexer.Queue(ctx, []string{"acme/one"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("background provider did not start")
	}
	configuration.Store(2)
	waitContext, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	canceled := make(chan error, 1)
	go func() { _, err := indexer.Reindex(waitContext, []string{"acme/one"}); canceled <- err }()
	select {
	case err := <-canceled:
		t.Fatalf("explicit request returned before the running provider completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	stopWaiting()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting request ignored cancellation: %v", err)
	}
	one, two := make(chan error, 1), make(chan error, 1)
	go func() { _, err := indexer.Reindex(ctx, []string{"acme/one"}); one <- err }()
	go func() { _, err := indexer.Reindex(ctx, []string{"acme/two"}); two <- err }()
	select {
	case err := <-two:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("unrelated package waited for the busy provider")
	}
	select {
	case err := <-one:
		t.Fatalf("explicit request returned before publication: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-one; err != nil {
		t.Fatalf("explicit request did not apply the newer configuration: %v", err)
	}
	indexer.backgroundWait.Wait()
	if calls.Load() != 2 || observed.Load() != 2 || len(indexer.pending) != 0 || len(indexer.running) != 0 {
		t.Fatalf("fresh configuration was not applied: calls=%d observed=%d pending=%v", calls.Load(), observed.Load(), indexer.pending)
	}
}

type monitorDatabaseStub struct {
	sharedStateDatabaseStub
	unavailable atomic.Bool
}

func (s *monitorDatabaseStub) Check(context.Context) (database.Status, error) {
	if s.unavailable.Load() {
		return database.Status{}, errors.New("database unavailable")
	}
	return database.Status{State: database.StateReady}, nil
}

type monitorGate chan bool

func (g monitorGate) SetAvailable(available bool, _ string) { g <- available }

func TestRevisionMonitorContinuesDuringProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(root, "revision.db"), MaximumOpenConnections: 4})
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	follower, err := packages.NewIndexRevisionFollower(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(id string, revision int) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('indexes', $1, ''), ($2, $1, '') ON CONFLICT ("domain") DO UPDATE SET "revision" = excluded."revision"`, revision, "index:"+id); err != nil {
			t.Fatal(err)
		}
	}
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{}}
	for _, id := range []string{"acme/one", "acme/two"} {
		source.entries[id] = packages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
	}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	one, two, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(waiting) })
	defer release()
	var startOne, startTwo sync.Once
	var oneCalls atomic.Int32
	runner := indexJobFunc(func(ctx context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		fragments := map[string]any{}
		for _, selected := range options.Arguments[1].(serviceIndexScope).Packages {
			if selected.PackageID == "acme/one" {
				oneCalls.Add(1)
				startOne.Do(func() { close(one) })
				select {
				case <-waiting:
				case <-ctx.Done():
					return jobs.Record{}, ctx.Err()
				}
			} else {
				startTwo.Do(func() { close(two) })
			}
			fragments[selected.PackageID] = map[string]any{"services": []any{}}
		}
		return jobs.Record{State: "SUCCEEDED", Result: map[string]any{"packages": fragments}}, nil
	})
	indexer := &runtimeIndexer{handlers: &handlerIndexerStub{}, commands: &commandIndexerStub{}, packages: store, jobs: runner, services: webservices.NewIndex()}
	indexer.background, indexer.cancel = context.WithCancel(ctx)
	defer indexer.Close()
	shared := &runtimeSharedState{indexes: follower, reindex: indexer.Reindex, retry: indexer.RetryPending, queue: indexer.Queue, queuePending: indexer.QueuePending}
	health, gate := &monitorDatabaseStub{}, make(monitorGate, 16)
	ticks, stopped := make(chan time.Time, 1), make(chan struct{})
	go func() {
		defer close(stopped)
		monitorSharedState(ctx, ticks, health, &sharedSettingsStub{}, shared, gate, nil)
	}()
	defer func() { release(); cancel(); <-stopped }()
	publish("acme/one", 1)
	ticks <- time.Now()
	select {
	case <-one:
	case <-ctx.Done():
		t.Fatal("first provider did not start")
	}
	publish("acme/two", 2)
	ticks <- time.Now()
	select {
	case <-two:
	case <-time.After(time.Second):
		t.Fatal("periodic revision monitor waited behind another package's provider")
	}
	// Health changes must still gate and restore traffic while the first job runs.
	for _, unavailable := range []bool{true, false} {
		health.unavailable.Store(unavailable)
		ticks <- time.Now()
		for {
			select {
			case available := <-gate:
				if available != unavailable {
					goto observed
				}
			case <-ctx.Done():
				t.Fatal("health monitoring stopped during indexing")
			}
		}
	observed:
	}
	if oneCalls.Load() != 1 {
		t.Fatalf("monitor duplicated the running provider: %d calls", oneCalls.Load())
	}
}

func TestQueuedIndexingBoundsWorkRetainsOverflowAndJoinsOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{}}
	for n := range 17 {
		id := fmt.Sprintf("acme/package%d", n)
		source.entries[id] = packages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
	}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	waiting, started := make(chan struct{}), make(chan struct{}, 17)
	indexer := &runtimeIndexer{handlers: &handlerIndexerStub{}, commands: &commandIndexerStub{}, packages: store, services: webservices.NewIndex(), jobs: indexJobFunc(func(ctx context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		started <- struct{}{}
		select {
		case <-waiting:
		case <-ctx.Done():
			return jobs.Record{}, ctx.Err()
		}
		return jobs.Record{State: "SUCCEEDED", Result: options.Arguments[2]}, nil
	})}
	indexer.background, indexer.cancel = context.WithCancel(ctx)
	defer indexer.Close()
	for n := range 17 {
		_, err := indexer.Queue(ctx, []string{fmt.Sprintf("acme/package%d", n)})
		if n < 16 && err != nil || n == 16 && (err == nil || !strings.Contains(err.Error(), "capacity reached")) {
			t.Fatalf("admission %d: %v", n, err)
		}
	}
	if err := indexer.QueuePending(ctx); err == nil || !strings.Contains(err.Error(), "capacity reached") {
		t.Fatalf("retry exceeded active capacity: %v", err)
	}
	indexer.mu.Lock()
	running, pending, batches := len(indexer.running), len(indexer.pending), indexer.backgroundJobs
	indexer.mu.Unlock()
	if running != 16 || pending != 17 || batches != 16 {
		t.Fatalf("running=%d pending=%d batches=%d", running, pending, batches)
	}
	close(waiting)
	indexer.backgroundWait.Wait()
	if len(indexer.pending) != 1 || !indexer.pending["acme/package16"] {
		t.Fatalf("overflow request was lost: %v", indexer.pending)
	}
	if err := indexer.QueuePending(ctx); err != nil {
		t.Fatal(err)
	}
	indexer.backgroundWait.Wait()
	if len(indexer.pending) != 0 || len(indexer.running) != 0 || indexer.backgroundJobs != 0 {
		t.Fatal("completed background work retained ownership")
	}
	for range 17 {
		<-started
	}
	waiting = make(chan struct{})
	if _, err := indexer.Queue(ctx, []string{"acme/package0"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("provider did not start before shutdown")
	}
	if err := indexer.Close(); err != nil {
		t.Fatal(err)
	}
	if len(indexer.running) != 0 || indexer.backgroundJobs != 0 || !indexer.pending["acme/package0"] {
		t.Fatal("shutdown failed to join work and retain its unfinished owner")
	}
	if _, err := indexer.Queue(ctx, []string{"acme/package0"}); err == nil {
		t.Fatal("closed indexer admitted background work")
	}
}

func TestServiceIndexPublicationIsIndependentOfRuntimeStartup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{}}
	for _, id := range []string{"acme/one", "acme/two"} {
		source.entries[id] = packages.PackageIndex{PackageID: id, State: "ready", ActiveCommit: "first"}
	}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(id string) webservices.Specification {
		return webservices.Specification{
			ServiceID: id, Version: 1, CodeRevision: "first", Enabled: true,
			EntrypointURL: "file:///workspace/packages/acme/one/service.ts",
			Access:        webservices.AccessPolicy{Mode: "public"},
			Effective: webservices.Configuration{
				Execution: webservices.ExecutionConfiguration{AnonymousUser: "system"},
				Lifecycle: webservices.LifecycleConfiguration{ServiceType: "stateless", SessionKeepAlive: time.Minute},
				Scaling:   webservices.ScalingConfiguration{ConcurrencyPerWorker: 1, TargetUtilization: 1, WorkerKeepAlive: time.Minute},
				Placement: webservices.PlacementConfiguration{WorkersPerSandbox: 1},
				Timeouts:  webservices.TimeoutConfiguration{Request: time.Second, Drain: time.Second},
			},
		}
	}
	drafts := map[string][]webservices.Specification{
		"acme/one": {spec("acme/one/a"), spec("acme/one/b")},
		"acme/two": {spec("acme/two/keep")},
	}
	fail := ""
	calls := 0
	runner := indexJobFunc(func(_ context.Context, id, entry string, options jobs.Options) (jobs.Record, error) {
		calls++
		if id != "hooks/index-services" || entry != packages.HookDispatcherEntrypoint || len(options.Arguments) != 3 {
			t.Fatalf("not one ordinary hook invocation: %s %s %#v", id, entry, options)
		}
		fragments := map[string]any{}
		for _, selected := range options.Arguments[1].(serviceIndexScope).Packages {
			if selected.PackageID == fail {
				fragments[selected.PackageID] = map[string]any{"error": "provider failed", "services": []any{}}
			} else {
				fragments[selected.PackageID] = map[string]any{"services": drafts[selected.PackageID]}
			}
		}
		return jobs.Record{State: "SUCCEEDED", ReleaseID: options.ReleaseID, Result: map[string]any{"packages": fragments}}, nil
	})
	runtime := &targetedServiceReconcilerStub{fail: "acme/one/a"}
	indexer := &runtimeIndexer{packages: store, jobs: runner, services: webservices.NewIndex(), runtime: runtime}
	applied, diagnostics, err := indexer.indexServices(ctx, nil, false)
	if err != nil || len(applied) != 2 || len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "fragment accepted") || len(runtime.calls) != 3 || calls != 1 {
		t.Fatalf("applied=%v diagnostics=%v calls=%v error=%v", applied, diagnostics, runtime.calls, err)
	}
	before := calls
	if err := indexer.RetryPending(ctx); err != nil || calls != before {
		t.Fatal("runtime error reran the provider job")
	}
	fail = "acme/one"
	drafts[fail] = []webservices.Specification{spec("acme/one/b")}
	if applied, _, err := indexer.indexServices(ctx, nil, false); err == nil || !reflect.DeepEqual(applied, []string{"acme/two"}) {
		t.Fatalf("failed package blocked healthy package: applied=%v err=%v", applied, err)
	}
	if _, err := indexer.services.ReadService("acme/one/a"); err != nil {
		t.Fatal("failed chain removed accepted service")
	}
	fail = ""
	runtime.fail = "acme/one/a" // Retirement warning also cannot unpublish the fragment.
	applied, diagnostics, err = indexer.indexServices(ctx, []string{"acme/one"}, false)
	if err != nil || len(applied) != 1 || len(diagnostics) != 1 {
		t.Fatalf("applied=%v diagnostics=%v error=%v", applied, diagnostics, err)
	}
	if _, err := indexer.services.ReadService("acme/one/a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("omitted service survived publication")
	}
	if _, err := indexer.services.ReadService("acme/two/keep"); err != nil {
		t.Fatal("targeted publication changed unrelated fragment")
	}
	// A provider revision change invalidates every owner through the same path.
	folder := filepath.Join(root, "acme/one")
	for name, content := range map[string]string{
		"hooks/index.toml":            "hook = \"index-services\"\ndescription = \"Provider\"\nprogram = \"acme/one/index\"\n",
		"programs/index/program.toml": "schema = 1\ndescription = \"Provider\"\n",
		"programs/index/program.ts":   "export default () => {};",
	} {
		path := filepath.Join(folder, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReindexHandlers(ctx, "acme/one"); err != nil {
		t.Fatal(err)
	}
	applied, _, err = indexer.indexServices(ctx, []string{"acme/one"}, false)
	if err != nil || !reflect.DeepEqual(applied, []string{"acme/one", "acme/two"}) {
		t.Fatalf("provider change selected %v: %v", applied, err)
	}
	for _, invalid := range []map[string]any{
		{"services": []any{map[string]any{"unexpected": true}}},
		{"services": []any{}, "error": ""},
	} {
		indexer.jobs = indexJobFunc(func(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
			return jobs.Record{State: "SUCCEEDED", Result: map[string]any{"packages": map[string]any{
				"acme/one": invalid,
				"acme/two": map[string]any{"services": drafts["acme/two"]},
			}}}, nil
		})
		if applied, _, err := indexer.indexServices(ctx, nil, false); err == nil || !reflect.DeepEqual(applied, []string{"acme/two"}) {
			t.Fatalf("invalid specification was not isolated: applied=%v error=%v", applied, err)
		}
	}

	indexer.jobs = indexJobFunc(func(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		return jobs.Record{State: "SUCCEEDED", Result: map[string]any{"packages": map[string]any{
			"acme/foreign": map[string]any{"services": []any{}},
		}}}, nil
	})
	if _, _, err := indexer.indexServices(ctx, []string{"acme/one"}, false); err == nil {
		t.Fatal("hook changed its publication scope")
	}
	if _, err := indexer.services.ReadService("acme/one/b"); err != nil {
		t.Fatal("invalid result erased the accepted fragment")
	}

}

func TestCanceledServicePublicationRetainsEveryUnprocessedOwnerForRetry(t *testing.T) {
	root := t.TempDir()
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{
		"acme/one": {PackageID: "acme/one", State: "ready", ActiveCommit: "first"},
		"acme/two": {PackageID: "acme/two", State: "ready", ActiveCommit: "first"},
	}}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	indexer := &runtimeIndexer{packages: store, services: webservices.NewIndex(), jobs: indexJobFunc(func(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		calls++
		return jobs.Record{State: "SUCCEEDED", ReleaseID: options.ReleaseID, Result: options.Arguments[2]}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = indexer.indexServices(ctx, nil, false)
	var publication *indexPublicationError
	if !errors.As(err, &publication) || len(indexer.pending) != 2 || calls != 0 {
		t.Fatalf("pending=%v calls=%d error=%v", indexer.pending, calls, err)
	}
	if err := indexer.RetryPending(context.Background()); err != nil || len(indexer.pending) != 0 || calls != 1 {
		t.Fatalf("pending=%v calls=%d error=%v", indexer.pending, calls, err)
	}
}
