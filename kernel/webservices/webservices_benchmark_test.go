package webservices

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func BenchmarkWarmServiceDispatch(b *testing.B) {
	root := b.TempDir()
	store := writeCanonicalTestService(b, root, "the8020/demo/variables", 1, 1, 1, 1, "stateless")
	pools, router := newFakePools(), &fakeRouter{}
	manager := newTestManager(b, store, pools, router, filepath.Join(root, "node", "kernel", "services"))
	status, err := manager.Reconcile(b.Context(), "the8020/demo/variables")
	if err != nil || len(status.Sandboxes) != 1 {
		b.Fatalf("start=%#v err=%v", status, err)
	}
	poolID := status.Sandboxes[0].PoolID
	pools.mu.Lock()
	pools.capacityCalls[poolID], pools.ensureCalls[poolID] = 0, 0
	pools.events = nil
	pools.mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response := httptest.NewRecorder()
		manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/the8020/demo/variables/warm", nil))
		if response.Code != http.StatusOK {
			b.Fatalf("warm response = %d", response.Code)
		}
	}
	b.StopTimer()
	pools.mu.Lock()
	capacityCalls, ensureCalls := pools.capacityCalls[poolID], pools.ensureCalls[poolID]
	lifecycleEvents := len(pools.events)
	pools.mu.Unlock()
	b.ReportMetric(float64(capacityCalls)/float64(b.N), "capacity_reads/op")
	b.ReportMetric(float64(ensureCalls)/float64(b.N), "reconciles/op")
	b.ReportMetric(float64(lifecycleEvents)/float64(b.N), "lifecycle_events/op")
}
