package webservices

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"the8020/kernel/database"
	executionservices "the8020/kernel/execution/services"
	"the8020/kernel/packages"
)

func TestSourceRestartAcrossNodesDrainsOnceAndHardRestartKillsEveryGeneration(t *testing.T) {
	ctx := context.Background()
	db := database.New(database.Config{Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"), MaximumOpenConnections: 4})
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE "the8020__system__revisions" ("domain" TEXT PRIMARY KEY, "revision" INTEGER NOT NULL, "updatedAt" TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	const affected, unrelated = "acme/consumer/api", "acme/other/api"
	var nodes []*Manager
	var followers []*packages.IndexRevisionFollower
	var pools []*fakePools
	var originals []string
	for n := range 2 {
		root := t.TempDir()
		placement := func(spec *Specification) {
			spec.Effective.Placement.MinimumSandboxes = 2
			spec.Effective.Scaling.MinimumWorkers = 2
		}
		index := newTestServiceIndex(t, root, affected, placement)
		newTestServiceIndex(t, root, unrelated, placement)
		pool := newFakePools()
		manager := newTestManager(t, index, pool, &fakeRouter{}, filepath.Join(root, "observed"))
		manager.database = db
		manager.nodeID = []string{"nod-aaaaaaaaaa", "nod-bbbbbbbbbb"}[n]
		manager.nodes = &fakeNodeRouter{local: manager.nodeID, indexes: []int{n}}
		if err := manager.ReconcileAll(ctx); err != nil {
			t.Fatal(err)
		}
		initial, _ := manager.Inspect(affected)
		if len(initial.Sandboxes) != 1 || initial.Sandboxes[0].Index != n {
			t.Fatalf("node placement=%#v", initial.Sandboxes)
		}
		pool.occupiedSlots[initial.Sandboxes[0].PoolID] = 1
		originals = append(originals, initial.Sandboxes[0].WorkerIDs[0])
		manager.matchImports = func(_ context.Context, sandbox string, ids, paths []string) ([]string, error) {
			if !slices.Equal(paths, []string{"/workspace/packages/acme/shared/value.ts"}) {
				t.Fatalf("paths = %v", paths)
			}
			for _, record := range pool.records {
				if record.SandboxID == sandbox && record.LogicalServiceID == affected {
					if record.State == "DRAINING" {
						t.Error("scanned draining generation")
					}
					return ids, nil
				}
			}
			return nil, nil
		}
		follower, err := packages.NewIndexRevisionFollower(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		nodes, pools, followers = append(nodes, manager), append(pools, pool), append(followers, follower)
	}
	update := packages.PackageSetUpdate{Revision: 7, Paths: []string{"/workspace/packages/acme/shared/value.ts"}}
	var concurrent sync.WaitGroup
	failures := make(chan error, len(nodes))
	for _, node := range nodes {
		concurrent.Go(func() {
			failures <- packages.ReactToSourceUpdate(ctx, update, node.MatchingImports, func(ctx context.Context, id string, revision uint64) error {
				return node.RequestRestart(ctx, id, "soft", revision)
			})
		})
	}
	concurrent.Wait()
	for range nodes {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	for n, node := range nodes {
		notification, err := followers[n].Poll(ctx)
		if err != nil || !slices.Equal(notification.Restarts, []string{affected}) {
			t.Fatalf("notification=%#v error=%v", notification, err)
		}
		if _, err := node.RefreshRestart(ctx, affected); err != nil {
			t.Fatal(err)
		}
		status, err := node.Inspect(affected)
		if err != nil || status.LoadedVersion != 2 || status.VersionCount != 2 {
			t.Fatalf("soft=%#v error=%v", status, err)
		}
		if _, err := node.Restart(ctx, affected, "soft", 7); err != nil {
			t.Fatal(err)
		}
		unchanged, _ := node.Inspect(unrelated)
		if unchanged.LoadedVersion != 1 {
			t.Fatalf("unrelated=%#v", unchanged)
		}
		live, _ := pools[n].ListForService(affected)
		if len(live) != 2 {
			t.Fatalf("duplicate update created pools: %#v", live)
		}
		for _, record := range live {
			pools[n].occupiedSlots[record.ServiceID] = 1
		}
		if err := followers[n].Acknowledge(notification.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nodes[0].Restart(ctx, affected, "hard", 0); err != nil {
		t.Fatal(err)
	}
	for n, node := range nodes {
		if _, err := node.RefreshRestart(ctx, affected); err != nil {
			t.Fatal(err)
		}
		live, _ := pools[n].ListForService(affected)
		if len(live) != 1 || live[0].Generation != 3 || live[0].RestartRevision != 2 || slices.Contains(live[0].WorkerIDs, originals[n]) {
			t.Fatalf("hard pools=%#v", live)
		}
		unaffected, _ := pools[n].ListForService(unrelated)
		if len(unaffected) != 1 || unaffected[0].Generation != 1 {
			t.Fatalf("unrelated pools=%#v", unaffected)
		}
		// Reconstructing local lifecycle state must not kill the already fresh pool.
		node.hardRestarted = sync.Map{}
		if err := node.loadRestarts(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := node.RefreshRestart(ctx, affected); err != nil {
			t.Fatal(err)
		}
		after, _ := pools[n].ListForService(affected)
		if len(after) != 1 || after[0].ServiceID != live[0].ServiceID {
			t.Fatalf("replayed hard restart: %#v", after)
		}
	}
}

func TestSoftRestartRetainsOldCapacityOnStartupFailure(t *testing.T) {
	root := t.TempDir()
	const id = "acme/consumer/api"
	index := newTestServiceIndex(t, root, id, func(spec *Specification) { spec.Effective.Scaling.MinimumWorkers = 0 })
	pool := newFakePools()
	manager := newTestManager(t, index, pool, &fakeRouter{}, filepath.Join(root, "observed"))
	initial, err := manager.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate demand-created capacity with a zero Worker minimum.
	record, err := pool.Scale(context.Background(), initial.Sandboxes[0].PoolID, 1)
	if err != nil {
		t.Fatal(err)
	}
	manager.services[id].sandboxes[0].status.WorkerIDs = record.WorkerIDs
	index.setRestart(id, RestartRevision{Revision: 1})
	pool.failVersion[2] = errors.Join(executionservices.ErrInvalidServiceDefinition, errors.New("broken dependency"))
	failed, err := manager.Reconcile(context.Background(), id)
	if err == nil || failed.LoadedVersion != 1 {
		t.Fatalf("failed=%#v error=%v", failed, err)
	}
	if old, _ := pool.Inspect(record.ServiceID); len(old.WorkerIDs) != 1 || old.State != "READY" {
		t.Fatalf("old capacity=%#v", old)
	}
	index.setRestart(id, RestartRevision{Revision: 2})
	ready, err := manager.Reconcile(context.Background(), id)
	if err != nil || ready.LoadedVersion != 3 || ready.WorkerCount != 1 {
		t.Fatalf("retry=%#v error=%v", ready, err)
	}
}
