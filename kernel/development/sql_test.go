package development

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSandboxSQLRequiresItsOwnersToken(t *testing.T) {
	platform := newTestPlatform(t)
	for _, user := range []string{"alice", "bob"} {
		if _, err := platform.manager.Create(context.Background(), user); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := platform.manager.loadSandbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	platform.manager.config.ActivationGateway = nil
	platform.manager.config.SQL = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusOK)
	})
	for _, test := range []struct {
		method string
		user   string
		token  string
		status int
	}{
		{http.MethodPost, "alice", "", 401},
		{http.MethodPost, "alice", "wrong", 401},
		{http.MethodPost, "bob", alice.Token, 401},
		{http.MethodGet, "alice", alice.Token, 405},
		{http.MethodPost, "alice", alice.Token, 200},
	} {
		request := httptest.NewRequest(test.method, "/v1/development/sandboxes/"+test.user+"/sql", strings.NewReader(`{"statement":"SELECT 1"}`))
		request.Header.Set("Authorization", "Bearer "+test.token)
		response := httptest.NewRecorder()
		platform.manager.serveSandbox(response, request)
		if response.Code != test.status {
			t.Fatalf("%s %s: HTTP %d; want %d", test.method, test.user, response.Code, test.status)
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized requests reached SQL: %d calls", calls)
	}
	platform.manager.config.SQL = nil
	request := httptest.NewRequest(http.MethodPost, "/v1/development/sandboxes/alice/sql", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+alice.Token)
	response := httptest.NewRecorder()
	platform.manager.serveSandbox(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing SQL provider = HTTP %d", response.Code)
	}
}
