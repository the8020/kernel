package callback

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/state"
)

func TestConsoleUsesCatalogParsingValidationAndActiveCaller(t *testing.T) {
	store, _ := state.New(t.TempDir())
	spec, _ := callbackFixtureForWorkload(t, store, "service")
	registry := core.NewRegistry(nil)
	calls := 0
	for _, command := range []core.Command{
		{Version: 1, ID: "kernel.test", Name: "kernel.test", Path: []string{"kernel.test"}, MutatesState: true, Parameters: []core.Parameter{{Name: "value", Type: "string", Position: 1, Required: true}}},
		{Version: 1, ID: "kernel.secret", Name: "kernel.secret", Path: []string{"kernel.secret"}, Secrets: []core.SecretInput{{Name: "token", FileOption: "token-file"}}},
	} {
		if err := registry.Register(command, func(ctx context.Context, request core.Request) (core.Result, error) {
			caller, ok := execution.CallerFromContext(ctx)
			if !ok || caller.User.Username != "system" {
				t.Fatal("lost trusted caller")
			}
			calls++
			return core.Result{"value": request.Arguments["value"]}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := newCallbackTestServer(t, store, func(config *Config) { config.AdminBus = registry; config.Operations = &recordingOperations{} })
	invoke := func(operation string, input map[string]any, active bool) (int, operationCallResult) {
		payload := operationCallPayload{WorkerID: "wrk-1111111111", ContextID: "ctx-1111111111", User: execution.SystemUser(), Operation: operation, Input: input}
		if !active {
			payload.ContextID = ""
		}
		response := runtimeControlCall(t, server, spec, "/v1/runtime/operation/execute", protocol.MessageAdminCommand, payload)
		var envelope protocol.Envelope
		var result operationCallResult
		if response.Code == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(envelope.Payload, &result); err != nil {
				t.Fatal(err)
			}
		}
		return response.Code, result
	}
	_, catalog := invoke("admin.catalog", map[string]any{}, true)
	if !catalog.Success || len(catalog.Result.(map[string]any)["commands"].([]any)) != 2 {
		t.Fatal(catalog)
	}
	for _, test := range []struct {
		line, text string
		success    bool
	}{
		{`kernel.test "two words"`, `"value": "two words"`, true},
		{`kernel.test`, "invalid_arguments", false},
		{`missing`, "unknown command", false},
		{`kernel.secret --token-file /etc/passwd`, "secure-input commands", false},
		{`kernel.test "unterminated`, "quote", false},
		{strings.Repeat("x", 16385), "16 KiB", false},
	} {
		_, response := invoke("admin.line", map[string]any{"line": test.line}, true)
		if !response.Success {
			t.Fatal(response)
		}
		result := response.Result.(map[string]any)
		if !strings.Contains(result["output"].(string), test.text) || (result["exit_code"] == float64(0)) != test.success {
			t.Fatal(result)
		}
	}
	if status, _ := invoke("admin.line", map[string]any{"line": "kernel.test no"}, false); status != http.StatusBadRequest {
		t.Fatal(status)
	}
	if calls != 1 {
		t.Fatalf("rejected calls reached handler: %d", calls)
	}
}

func TestConsoleOutputIsBounded(t *testing.T) {
	var output consoleOutput
	input := []byte(strings.Repeat("é", 40000))
	if n, err := output.Write(input); n != len(input) || err != nil {
		t.Fatal(n, err)
	}
	_, _ = output.Write([]byte("later"))
	if output.Len() != 64<<10 || !output.truncated {
		t.Fatal("output was not bounded")
	}
}
