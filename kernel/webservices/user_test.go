package webservices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestServicePrincipalsNeedNoDatabase(t *testing.T) {
	for _, username := range []string{"system", "visitor"} {
		t.Run(username, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			const serviceID = "example/test/api"
			store := newTestServiceIndex(t, root, serviceID, func(spec *Specification) { spec.Effective.Execution.AnonymousUser = username })
			pools := newFakePools()
			pools.dispatched = make(chan dispatchedRequest, 1)
			manager := newTestManager(t, store, pools, &fakeRouter{}, filepath.Join(root, "observed"))
			if _, err := manager.Reconcile(ctx, serviceID); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Reconcile(ctx, serviceID); err != nil {
				t.Fatalf("account state affected principal: %v", err)
			}
			response := httptest.NewRecorder()
			manager.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+serviceID+"/", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP status %d", response.Code)
			}
			forwarded := <-pools.dispatched
			if forwarded.header.Get("the8020-internal-username") != username {
				t.Fatal("HTTP principal changed")
			}
			response = httptest.NewRecorder()
			manager.ServeHTTP(response, websocketRequest("/"+serviceID+"/", ""))
			if response.Code != http.StatusOK || pools.websockets[len(pools.websockets)-1].header.Get("the8020-internal-username") != username {
				t.Fatal("WebSocket principal changed")
			}
		})
	}
}
