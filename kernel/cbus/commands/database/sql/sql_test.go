package sql_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	databasesql "the8020/kernel/cbus/commands/database/sql"
	"the8020/kernel/database"
	"the8020/kernel/services"
)

func sqlHandler(t *testing.T) http.Handler {
	t.Helper()
	manager := database.New(database.Config{
		Backend: database.BackendSQLite, Location: filepath.Join(t.TempDir(), "system.db"),
		MaximumOpenConnections: 3, MaximumIdleConnections: 1, MaximumResultRows: 2,
	})
	t.Cleanup(func() { _ = manager.Close() })
	return databasesql.NewHTTP(&services.Services{Database: manager})
}

func TestHTTPSQLUsesBoundedNativeSQLWithoutApplicationInitialization(t *testing.T) {
	handler := sqlHandler(t)
	for _, test := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"statement":"CREATE TABLE sample (id INTEGER, name TEXT)","execute":true}`, 200, `"rows_affected":0`},
		{`{"statement":"INSERT INTO sample VALUES ($1, $2)","parameters":"[9007199254740993,\"O'Reilly\"]","execute":true}`, 200, `"rows_affected":1`},
		{`{"statement":"SELECT id AS value, name AS value FROM sample"}`, 200, `"rows":[[9007199254740993,"O'Reilly"]]`},
		{`{"statement":"SELECT 1","parameters":"[{}]"}`, 400, "parameter 1:"},
		{`{"statement":"SELECT 1","parameters":[1]}`, 400, "invalid SQL request"},
		{`{"statement":"SELECT 1","user":"system"}`, 400, "invalid SQL request"},
		{`{"statement":"SELECT 1"} {}`, 400, "invalid SQL request"},
		{`{"statement":"SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3"}`, 500, "exceeds 2 rows"},
		{`{"statement":"SELECT * FROM missing"}`, 500, "missing"},
		{`{"statement":"` + strings.Repeat("x", 1<<20) + `"}`, 400, "invalid SQL request"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/sql", strings.NewReader(test.body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.want) {
			t.Fatalf("SQL response = %d %s; want %d containing %q", response.Code, response.Body, test.status, test.want)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("SQL response may be cached")
		}
	}
}

func TestSandboxSQLCLI(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("Deno is required for the sandbox SQL helper check")
	}
	handler := sqlHandler(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/development/sandboxes/developer/sql" || request.Header.Get("Authorization") != "Bearer test-sandbox-token" {
			http.Error(writer, "invalid sandbox request", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	script, err := filepath.Abs("../../../../../../dev-skills/the8020-dev-db/scripts/sql.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
		fail bool
	}{
		{[]string{"SELECT $1 AS value", "--parameters", "[9007199254740993]"}, `"rows":[[9007199254740993]]`, false},
		{[]string{"--execute", "CREATE TABLE example (value TEXT)"}, `"rows_affected":0`, false},
		{[]string{"--execute", "INSERT INTO example VALUES ($1)", "--parameters", `["hello"]`}, `"rows_affected":1`, false},
		{[]string{"SELECT value FROM example"}, `"rows":[["hello"]]`, false},
		{[]string{"--", "-- SQL comment\nSELECT 7 AS value"}, `"rows":[[7]]`, false},
		{[]string{"SELECT 1", "--parameters", "[{}]"}, "SQL failed (400)", true},
		{[]string{"SELECT * FROM missing"}, "SQL failed (500)", true},
		{[]string{"SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3"}, "exceeds 2 rows", true},
		{[]string{"SELECT 1", "--parameters"}, "requires a JSON array", true},
		{[]string{"SELECT 1", "SELECT 2"}, "Expected one SQL statement", true},
		{nil, "A SQL statement is required", true},
		{[]string{"--help"}, "Usage: sql", false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, deno, append([]string{"run", "--allow-env", "--allow-net", script}, test.args...)...)
		command.Env = append(os.Environ(), "DEVELOPMENT_ACTIVATION_ENDPOINT="+server.URL, "DEVELOPMENT_ACTIVATION_TOKEN=test-sandbox-token", "DEVELOPMENT_USER_ID=developer")
		output, err := command.CombinedOutput()
		cancel()
		if (err != nil) != test.fail || !strings.Contains(string(output), test.want) {
			t.Fatalf("sql %q: %v, %s; want failure=%t containing %q", test.args, err, output, test.fail, test.want)
		}
	}
}
