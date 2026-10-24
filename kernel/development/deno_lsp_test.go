package development

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func nativeDenoLanguageServer(t *testing.T, driver *RunscDriver, sandbox Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	defer send.Close()
	defer receive.Close()
	finished := make(chan error, 1)
	go func() {
		err := driver.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/bash", "-c", "cd /workspace; exec deno lsp 2>/tmp/deno-lsp.log"}, input, output)
		input.CloseWithError(err)
		output.CloseWithError(err)
		finished <- err
	}()
	defer func() { cancel(); send.Close(); receive.Close(); <-finished }()
	write := func(message any) {
		t.Helper()
		body, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(send, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(receive)
	id := 0
	request := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			message["params"] = params
		}
		write(message)
		for {
			header, err := textproto.NewReader(reader).ReadMIMEHeader()
			if err != nil {
				t.Fatalf("LSP header: %v", err)
			}
			size, err := strconv.Atoi(header.Get("Content-Length"))
			if err != nil || size < 0 || size > 1<<20 {
				t.Fatalf("LSP length: %v", header)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(reader, body); err != nil {
				t.Fatal(err)
			}
			var message struct {
				ID     *int
				Method string
				Result json.RawMessage
				Error  json.RawMessage
			}
			if err := json.Unmarshal(body, &message); err != nil {
				t.Fatal(err)
			}
			if message.Method != "" && message.ID != nil {
				write(map[string]any{"jsonrpc": "2.0", "id": *message.ID, "result": nil})
			} else if message.ID != nil && *message.ID == id {
				if len(message.Error) != 0 {
					t.Fatalf("LSP %s: %s", method, message.Error)
				}
				return message.Result
			}
		}
	}
	notify := func(method string, params any) {
		message := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			message["params"] = params
		}
		write(message)
	}
	request("initialize", map[string]any{"processId": nil, "rootUri": "file:///workspace", "capabilities": map[string]any{}, "initializationOptions": map[string]any{"enable": true, "lint": false, "config": "/workspace/deno.json"}})
	notify("initialized", map[string]any{})
	uri := "file:///workspace/editor-check.ts"
	text := "import { context } from \"@the8020/context\";\nimport { query } from \"/p/the8020/deno-types/shared.ts\";\ncontext.username; query.limit;\n"
	notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "typescript", "version": 1, "text": text}})
	definition := func(line, character int, want string) {
		t.Helper()
		result := request("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]int{"line": line, "character": character}})
		if !strings.Contains(string(result), want) {
			t.Fatalf("LSP definition wanted %s: %s", want, result)
		}
	}
	definition(2, 3, "file:///opt/runtime/context/")
	definition(2, 20, "file:///workspace/packages/the8020/deno-types/shared.ts")
	nativeExec(t, driver, sandbox, `set -eu
mkdir -p /workspace/packages/editor/private
printf '%s' '{"imports":{"local":"./value.ts"}}' >/workspace/packages/editor/private/deno.jsonc
printf 'export const value = 42;\n' >/workspace/packages/editor/private/value.ts
printf 'export { value } from "local";\n' >/workspace/packages/editor/private/mod.ts
`)
	notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": "file:///workspace/packages/editor/private/deno.jsonc", "type": 1}}})
	notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2}, "contentChanges": []any{map[string]any{"text": "import { value } from \"/p/editor/private/mod.ts\";\nvalue;\n"}}})
	definition(1, 2, "file:///workspace/packages/editor/private/value.ts")
	request("shutdown", nil)
	notify("exit", nil)
}
