package logs

import (
	"net/http/httptest"
	"strings"
	"testing"

	"the8020/kernel/services"
)

func TestSandboxLogsPinNodeAndValidateInput(t *testing.T) {
	handler := NewHTTP(&services.Services{Instance: services.InstanceInfo{UUID: "nod-0123456789"}})
	for _, test := range []struct {
		body     string
		status   int
		contains string
	}{
		{`{}`, 200, `"node_id":"nod-0123456789"`},
		{`{"text":"test"}`, 200, `"state":"unavailable"`},
		{`{"node_id":"nod-abcdefghij"}`, 400, "hosting node"},
		{`{"path":"/etc/passwd"}`, 400, "invalid log query"},
		{`{} {}`, 400, "invalid log query"},
		{`{"limit":501}`, 400, "limits"},
		{`{"text":"` + strings.Repeat("x", 1025) + `"}`, 400, "1024"},
		{`{"text":"` + strings.Repeat("x", 16385) + `"}`, 400, "invalid log query"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/logs", strings.NewReader(test.body)))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.contains) {
			t.Fatal(response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("logs are cacheable")
		}
	}
}
