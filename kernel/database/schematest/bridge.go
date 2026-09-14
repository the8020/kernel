// Package schematest runs actual db package schema code against native database authority in tests.
package schematest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"the8020/kernel/database"
)

func Attach(t testing.TB, manager *database.Manager) {
	t.Helper()
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("schema integration requires Deno on PATH")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../db"))
	if _, err := os.Stat(filepath.Join(root, "internal/schema_bridge_test.ts")); os.IsNotExist(err) {
		t.Skip("schema integration requires a sibling db source checkout")
	} else if err != nil {
		t.Fatal(err)
	}
	var sequence atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Operation string          `json:"operation"`
			Input     json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&call); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		scope := r.Header.Get("X-Test-Scope")
		var value any
		var err error
		switch call.Operation {
		case "database.info":
			value = manager.Status()
		case "database.execute":
			var input database.StatementRequest
			if err = json.Unmarshal(call.Input, &input); err == nil {
				value, err = manager.RunStatement(r.Context(), scope, input)
			}
		case "database.transaction.begin":
			var input struct {
				Settings database.TransactionSettings `json:"settings"`
			}
			if err = json.Unmarshal(call.Input, &input); err == nil {
				var token string
				token, err = manager.BeginTransaction(r.Context(), scope, input.Settings)
				value = map[string]string{"transaction": token}
			}
		case "database.transaction.commit", "database.transaction.rollback":
			var input struct {
				Transaction string `json:"transaction"`
			}
			if err = json.Unmarshal(call.Input, &input); err == nil {
				if call.Operation == "database.transaction.commit" {
					err = manager.FinishTransaction(r.Context(), scope, input.Transaction, true)
				} else {
					err = manager.FinishTransaction(r.Context(), scope, input.Transaction, false)
				}
			}
		default:
			err = errors.New("unexpected test bridge operation")
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	manager.SetSchemaExecutor(func(ctx context.Context, request database.SchemaRequest) (json.RawMessage, error) {
		encoded, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		scope := fmt.Sprintf("schema-test:%d", sequence.Add(1))
		defer manager.CloseScope(scope)
		command := exec.CommandContext(ctx, deno, "run", "--quiet", "--node-modules-dir=none", "--import-map=deno.local.json", "--allow-read", "--allow-net="+server.Listener.Addr().String(), "internal/schema_bridge_test.ts", server.URL, manager.Backend(), scope)
		command.Dir, command.Stdin = root, bytes.NewReader(encoded)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("schema package: %w: %s", err, stderr.String())
		}
		if !json.Valid(output) {
			return nil, errors.New("schema test returned invalid JSON")
		}
		return output, nil
	})
}
