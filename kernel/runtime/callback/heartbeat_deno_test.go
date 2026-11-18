package callback

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/sandbox/model"
	"the8020/kernel/sandbox/state"
)

func TestDenoSupervisorSnapshotsReachCallback(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires pinned Deno on PATH")
	}
	runtime, err := filepath.Abs("../../../defaults/config/runtime/deno")
	if err != nil {
		t.Fatal(err)
	}
	for _, workload := range []model.WorkloadType{model.WorkloadService, model.WorkloadJob} {
		t.Run(string(workload), func(t *testing.T) {
			store, _ := state.New(t.TempDir())
			spec, _ := callbackFixtureForWorkload(t, store, workload)
			var calls atomic.Int64
			base := time.Unix(1700000000, 0).UTC()
			server := newCallbackTestServer(t, store, func(c *Config) {
				c.Now = func() time.Time { return base.Add(time.Duration(calls.Add(1)) * time.Second) }
			})
			if err := server.Start(); err != nil {
				t.Fatal(err)
			}
			defer server.Close(context.Background())
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, deno, "run", "--frozen", "--cached-only", "--unstable-worker-options", "--allow-read="+runtime+","+server.Address(), "--allow-write="+server.Address(), "--allow-net", "--allow-import", "supervisor/heartbeat_fixture_test.ts", server.Address(), string(workload))
			command.Dir = runtime
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("real supervisor/Worker callback: %v\n%s", err, output)
			}
			snapshot, ok := store.Snapshot(spec.SandboxID)
			_, status, err := store.Load(spec.SandboxID)
			if err != nil || !ok || !status.SupervisorHealthy || !status.LastHeartbeat.Equal(base.Add(3*time.Second)) || calls.Load() != 4 {
				t.Fatalf("fresh/equal/stale/rejected heartbeat contract: status=%+v calls=%d err=%v", status, calls.Load(), err)
			}
			if snapshot.WorkerCount != 1 || len(snapshot.Workers) != 1 || snapshot.Workers[0].Resources == nil || snapshot.Workers[0].Resources.SampledAt.IsZero() {
				t.Fatalf("real Worker sample did not survive snapshot decoding: %+v", snapshot)
			}
		})
	}
}
