package development

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSandboxSQLRequiresItsOwnersToken(t *testing.T) {
	testSandboxToolToken(t, "sql")
}

func TestSandboxLogsRequiresItsOwnersToken(t *testing.T) {
	testSandboxToolToken(t, "logs")
}

func testSandboxToolToken(t *testing.T, operation string) {
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
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusOK)
	})
	if operation == "sql" {
		platform.manager.config.SQL = handler
	} else {
		platform.manager.config.Logs = handler
	}
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
		request := httptest.NewRequest(test.method, "/v1/development/sandboxes/"+test.user+"/"+operation, strings.NewReader(`{"statement":"SELECT 1"}`))
		request.Header.Set("Authorization", "Bearer "+test.token)
		response := httptest.NewRecorder()
		platform.manager.serveSandbox(response, request)
		if response.Code != test.status {
			t.Fatalf("%s %s: HTTP %d; want %d", test.method, test.user, response.Code, test.status)
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized requests reached sandbox tool: %d calls", calls)
	}
	platform.manager.config.SQL = nil
	platform.manager.config.Logs = nil
	request := httptest.NewRequest(http.MethodPost, "/v1/development/sandboxes/alice/"+operation, strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+alice.Token)
	response := httptest.NewRecorder()
	platform.manager.serveSandbox(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing tool provider = HTTP %d", response.Code)
	}
}
