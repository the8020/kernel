package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"the8020/kernel/console"
	"the8020/kernel/execution"
	"the8020/kernel/identity"
	"the8020/kernel/sandbox/backend"
	"the8020/kernel/webservices"
)

// SSH and browser sessions use the same package display owner and metadata.
// Authentication has already been approved by the native transport's adapter.
func namedTerminalOpener(services *webservices.Manager, consoles *console.Manager) func(context.Context, string, string, string, string, backend.ConsoleOptions) (backend.Console, error) {
	return func(ctx context.Context, username, kind, sandboxID, sessionID string, options backend.ConsoleOptions) (backend.Console, error) {
		user, err := execution.UserForUsername(username)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(map[string]any{
			"targetKind": kind, "targetSandboxId": sandboxID, "sessionId": sessionID,
			"arguments": options.Arguments, "environment": options.Environment,
			"workingDir": options.WorkingDir, "size": options.Size,
		})
		if err != nil {
			return nil, err
		}
		result, err := services.Request(ctx, "the8020/dev-core/terminals", http.MethodPost, "/open", webservices.RequestOptions{
			Body: bytes.NewReader(body), Headers: http.Header{"Content-Type": {"application/json"}},
			Timeout: 30 * time.Second, AuthenticatedUser: &user,
		})
		if err != nil {
			return nil, err
		}
		var response struct {
			Terminal struct {
				TerminalID string `json:"terminalId"`
			} `json:"terminal"`
		}
		if result.StatusCode != http.StatusOK || json.Unmarshal([]byte(result.Body), &response) != nil || !identity.Is(response.Terminal.TerminalID, "tty") {
			return nil, fmt.Errorf("named terminal service unavailable (HTTP %d)", result.StatusCode)
		}
		return consoles.OpenTerminalView(ctx, response.Terminal.TerminalID, sandboxID, options.Size)
	}
}
