package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/webservices"
	"time"

	"the8020/kernel/cbus/core"
	"the8020/kernel/cbus/discovery"
	"the8020/kernel/packages"
)

type handlerIndexerStub struct {
	calls [][]string
	err   error
}

func (s *handlerIndexerStub) ReindexHandlers(_ context.Context, ids ...string) (packages.HandlerReport, error) {
	s.calls = append(s.calls, append([]string(nil), ids...))
	return packages.HandlerReport{Events: 3, Hooks: 2}, s.err
}

type commandIndexerStub struct{ calls [][]string }

func (s *commandIndexerStub) Reindex(_ context.Context, ids ...string) (discovery.Report, error) {
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
	if err := shared.refreshPackages(context.Background()); err == nil || len(updates.acks) != 0 {
		t.Fatal("native failure was acknowledged")
	}
	fail = false
	if err := shared.refreshPackages(context.Background()); err != nil || calls != 2 || len(updates.acks) != 1 {
		t.Fatalf("calls=%d acks=%v error=%v", calls, updates.acks, err)
	}
	if err := shared.refreshPackages(context.Background()); err != nil || calls != 2 {
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
		"acme/one/index-services": {spec("acme/one/a"), spec("acme/one/b")},
		"acme/two/index-services": {spec("acme/two/keep")},
	}
	fail := ""
	calls := 0
	runner := indexJobFunc(func(_ context.Context, id, entry string, options jobs.Options) (jobs.Record, error) {
		calls++
		if id == fail {
			return jobs.Record{State: "FAILED", Failure: "hook acme/provider failed"}, nil
		}
		return jobs.Record{State: "SUCCEEDED", ReleaseID: options.ReleaseID, Result: map[string]any{"services": drafts[id]}}, nil
	})
	runtime := &targetedServiceReconcilerStub{fail: "acme/one/a"}
	indexer := &runtimeIndexer{packages: store, jobs: runner, services: webservices.NewIndex(), runtime: runtime}
	applied, diagnostics, err := indexer.indexServices(ctx, nil)
	if err != nil || len(applied) != 2 || len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "fragment accepted") || len(runtime.calls) != 3 {
		t.Fatalf("applied=%v diagnostics=%v calls=%v error=%v", applied, diagnostics, runtime.calls, err)
	}
	before := calls
	if err := indexer.RetryPending(ctx); err != nil || calls != before {
		t.Fatal("runtime error reran the provider job")
	}
	fail = "acme/one/index-services"
	drafts[fail] = []webservices.Specification{spec("acme/one/b")}
	if _, _, err := indexer.indexServices(ctx, []string{"acme/one"}); err == nil {
		t.Fatal("failed hook was accepted")
	}
	if _, err := indexer.services.ReadService("acme/one/a"); err != nil {
		t.Fatal("failed chain removed accepted service")
	}
	fail = ""
	runtime.fail = "acme/one/a" // Retirement warning also cannot unpublish the fragment.
	applied, diagnostics, err = indexer.indexServices(ctx, []string{"acme/one"})
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
	applied, _, err = indexer.indexServices(ctx, []string{"acme/one"})
	if err != nil || !reflect.DeepEqual(applied, []string{"acme/one", "acme/two"}) {
		t.Fatalf("provider change selected %v: %v", applied, err)
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
		return jobs.Record{State: "SUCCEEDED", ReleaseID: options.ReleaseID, Result: map[string]any{"services": []any{}}}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = indexer.indexServices(ctx, nil)
	var publication *indexPublicationError
	if !errors.As(err, &publication) || len(indexer.pending) != 2 || calls != 0 {
		t.Fatalf("pending=%v calls=%d error=%v", indexer.pending, calls, err)
	}
	if err := indexer.RetryPending(context.Background()); err != nil || len(indexer.pending) != 0 || calls != 2 {
		t.Fatalf("pending=%v calls=%d error=%v", indexer.pending, calls, err)
	}
}
