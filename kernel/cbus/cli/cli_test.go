package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"the8020/kernel/cbus/core"
)

type fakeExecutor struct{ request core.Request }

func (f *fakeExecutor) Execute(_ context.Context, request core.Request) (core.Response, error) {
	f.request = request
	return core.Response{ProtocolVersion: core.ProtocolVersion, Success: true, RequestID: request.RequestID, Result: core.Result{"ok": true}}, nil
}

type errorExecutor struct{ response core.Response }

func (e errorExecutor) Execute(context.Context, core.Request) (core.Response, error) {
	return e.response, nil
}

func testCatalog() []core.Command {
	return []core.Command{{ID: "thing.set", Path: []string{"thing", "set"}, Aliases: [][]string{{"config", "set"}}, Summary: "Set thing", Description: "Sets a thing.", Parameters: []core.Parameter{{Name: "name", Type: "string", Required: true}, {Name: "count", Type: "integer", Position: 1, Required: true}, {Name: "enabled", Type: "boolean", Position: 2, Required: true}, {Name: "namespace", Type: "string", Option: "namespace", Description: "Grouping namespace."}, {Name: "detached", Type: "boolean", Option: "detached", Description: "Return without waiting."}}, Examples: []string{"thing set x 2 true --namespace demo --detached"}}}
}

func TestTextErrorsRenderStructuredDetails(t *testing.T) {
	commandError := core.NewError(core.CodeRuntimeOperation, "one or more packages failed to synchronize")
	commandError.Details = map[string]any{"packages": []any{map[string]any{
		"package_id": "the8020/uui",
		"success":    false,
		"error":      "installed package has uncommitted changes",
	}}}
	runner := New(testCatalog(), errorExecutor{response: core.Response{
		ProtocolVersion: core.ProtocolVersion,
		Error:           commandError,
	}})
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"thing", "set", "value", "2", "true"}, false, &output); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	for _, expected := range []string{
		"error [runtime_operation_failed]: one or more packages failed to synchronize",
		"package_id: the8020/uui",
		"error: installed package has uncommitted changes",
		"success: false",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestSharedLookupParsingHelpAndRendering(t *testing.T) {
	executor := &fakeExecutor{}
	runner := New(testCatalog(), executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"config", "set", "value", "--namespace=demo", "2", "--detached", "true"}, false, &output); code != 0 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	if executor.request.CommandID != "thing.set" || executor.request.Arguments["count"] != int64(2) || executor.request.Arguments["enabled"] != true || executor.request.Arguments["namespace"] != "demo" || executor.request.Arguments["detached"] != true {
		t.Fatalf("request: %#v", executor.request)
	}
	if !strings.Contains(output.String(), "ok: true") {
		t.Fatalf("output: %s", output.String())
	}
	help := runner.Help([]string{"thing", "set"})
	for _, expected := range []string{"Usage: thing set <name> <count> <enabled> [--detached] [--namespace <namespace>]", "Options:", "--detached", "config set", "thing set x 2 true"} {
		if !strings.Contains(help, expected) {
			t.Errorf("help missing %q: %s", expected, help)
		}
	}
}

func TestGlobalHelpListsCatalogAndLocalCommands(t *testing.T) {
	runner := New(testCatalog(), &fakeExecutor{})
	help := runner.Help(nil)
	for _, expected := range []string{
		"exit                     Exit the interactive console",
		"help [command path]      Show commands or detailed help",
		"thing set                Set thing",
	} {
		if !strings.Contains(help, expected) {
			t.Errorf("global help missing %q:\n%s", expected, help)
		}
	}
	if exitIndex, helpIndex, thingIndex := strings.Index(help, "  exit"), strings.Index(help, "  help"), strings.Index(help, "  thing set"); exitIndex < 0 || helpIndex <= exitIndex || thingIndex <= helpIndex {
		t.Fatalf("help entries are not ordered:\n%s", help)
	}

	exitHelp := runner.Help([]string{"exit"})
	for _, expected := range []string{"Usage: exit", "Exit the interactive console", "without shutting down the kernel"} {
		if !strings.Contains(exitHelp, expected) {
			t.Errorf("exit help missing %q:\n%s", expected, exitHelp)
		}
	}
}

func TestArgumentAndLineErrors(t *testing.T) {
	runner := New(testCatalog(), &fakeExecutor{})
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"thing", "set", "x", "no", "true"}, false, &output); code != 2 || !strings.Contains(output.String(), "must be an integer") {
		t.Fatalf("code %d output %s", code, output.String())
	}
	if _, err := SplitLine("'unfinished"); err == nil {
		t.Fatal("accepted unfinished quote")
	}
	tokens, err := SplitLine(`thing set "hello world" 2 true`)
	if err != nil || tokens[2] != "hello world" {
		t.Fatalf("tokens %#v error %v", tokens, err)
	}
}

func TestNamedOptionErrorsAndTerminator(t *testing.T) {
	runner := New(testCatalog(), &fakeExecutor{})
	tests := []struct {
		tokens   []string
		contains string
	}{
		{[]string{"thing", "set", "x", "2", "true", "--missing"}, "unknown option"},
		{[]string{"thing", "set", "x", "2", "true", "--namespace"}, "requires a string value"},
		{[]string{"thing", "set", "x", "2", "true", "--detached", "--detached"}, "only be specified once"},
		{[]string{"thing", "set", "x", "2", "true", "--detached=no"}, "must be true or false"},
	}
	for _, test := range tests {
		var output bytes.Buffer
		if code := runner.Run(context.Background(), test.tokens, false, &output); code != 2 || !strings.Contains(output.String(), test.contains) {
			t.Errorf("tokens %v: code %d output %q", test.tokens, code, output.String())
		}
	}

	executor := &fakeExecutor{}
	runner = New([]core.Command{{ID: "test.run", Path: []string{"test", "run"}, Summary: "test", Description: "test", Parameters: []core.Parameter{{Name: "value", Type: "string", Required: true}}}}, executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"test", "run", "--", "--literal"}, false, &output); code != 0 || executor.request.Arguments["value"] != "--literal" {
		t.Fatalf("terminator: code %d request %#v output %q", code, executor.request, output.String())
	}
}

func TestMetadataDeclaredSecretInput(t *testing.T) {
	command := core.Command{
		ID: "auth.bootstrap_admin.add", Path: []string{"auth", "bootstrap-admin", "add"}, Summary: "add", Description: "add",
		Parameters: []core.Parameter{
			{Name: "username", Type: "string", Position: 0, Required: true},
			{Name: "password", Type: "string", Required: true, Secret: true, SecretPrompt: "Password: ", SecretConfirmationPrompt: "Confirm password: ", SecretStdinOption: "password-stdin", Description: "password"},
		},
	}
	executor := &fakeExecutor{}
	runner := New([]core.Command{command}, executor)
	var fromStdin bool
	runner.SetSecretResolver(func(prompt, confirmation string, stdin bool) (string, error) {
		if prompt != "Password: " || confirmation != "Confirm password: " {
			t.Fatalf("prompts = %q, %q", prompt, confirmation)
		}
		fromStdin = stdin
		return "not-in-command-history", nil
	})
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"auth", "bootstrap-admin", "add", "admin"}, false, &output); code != 0 || fromStdin {
		t.Fatalf("prompt run code=%d fromStdin=%t output=%q", code, fromStdin, output.String())
	}
	if executor.request.Arguments["password"] != "not-in-command-history" {
		t.Fatalf("resolved request = %#v", executor.request.Arguments)
	}
	if help := runner.Help(command.Path); !strings.Contains(help, "[--password-stdin]") || strings.Contains(help, "<password>") {
		t.Fatalf("secret help exposed an ordinary password argument:\n%s", help)
	}

	output.Reset()
	if code := runner.Run(context.Background(), []string{"auth", "bootstrap-admin", "add", "admin", "--password-stdin"}, false, &output); code != 0 || !fromStdin {
		t.Fatalf("stdin run code=%d fromStdin=%t output=%q", code, fromStdin, output.String())
	}
	output.Reset()
	if code := runner.Run(context.Background(), []string{"auth", "bootstrap-admin", "add", "admin", "ordinary-password"}, false, &output); code != 2 || !strings.Contains(output.String(), "too many arguments") {
		t.Fatalf("ordinary password run code=%d output=%q", code, output.String())
	}
}

func TestMetadataDeclaredValuePromptRunsBeforeSecretPrompt(t *testing.T) {
	command := core.Command{
		ID: "auth.bootstrap_admin.add", Path: []string{"auth", "bootstrap-admin", "add"}, Summary: "add", Description: "add",
		Parameters: []core.Parameter{
			{Name: "username", Type: "string", Position: 0, Required: true, Prompt: "Username: "},
			{Name: "password", Type: "string", Required: true, Secret: true, SecretPrompt: "Password: ", SecretConfirmationPrompt: "Confirm password: ", SecretStdinOption: "password-stdin"},
		},
	}
	executor := &fakeExecutor{}
	runner := New([]core.Command{command}, executor)
	var events []string
	runner.SetValueResolver(func(prompt string) (string, error) {
		events = append(events, prompt)
		return "prompted-admin", nil
	})
	runner.SetSecretResolver(func(prompt, _ string, _ bool) (string, error) {
		events = append(events, prompt)
		return "secure-password", nil
	})
	var output bytes.Buffer
	if code := runner.Run(context.Background(), command.Path, false, &output); code != 0 {
		t.Fatalf("prompted run code=%d output=%q", code, output.String())
	}
	if strings.Join(events, "|") != "Username: |Password: " {
		t.Fatalf("prompt order = %#v", events)
	}
	if executor.request.Arguments["username"] != "prompted-admin" || executor.request.Arguments["password"] != "secure-password" {
		t.Fatalf("prompted request = %#v", executor.request.Arguments)
	}

	events = nil
	output.Reset()
	if code := runner.Run(context.Background(), append(append([]string(nil), command.Path...), "explicit-admin"), false, &output); code != 0 {
		t.Fatalf("explicit run code=%d output=%q", code, output.String())
	}
	if strings.Join(events, "|") != "Password: " || executor.request.Arguments["username"] != "explicit-admin" {
		t.Fatalf("explicit prompt events=%#v request=%#v", events, executor.request.Arguments)
	}

	secretResolved := false
	runner = New([]core.Command{command}, executor)
	runner.SetSecretResolver(func(_, _ string, _ bool) (string, error) {
		secretResolved = true
		return "secure-password", nil
	})
	output.Reset()
	if code := runner.Run(context.Background(), command.Path, false, &output); code != 2 || !strings.Contains(output.String(), "missing <username>") || secretResolved {
		t.Fatalf("missing resolver code=%d secretResolved=%t output=%q", code, secretResolved, output.String())
	}
}

func TestCompactSummariesAndDetailedFieldOrder(t *testing.T) {
	compact := map[string]any{"settings": []any{
		map[string]any{"description": "Main HTTP listener port.", "key": "network.main_port"},
		map[string]any{"key": "logging.enabled", "description": "Whether logging is enabled."},
	}}
	var output bytes.Buffer
	renderValue(&output, compact, 0)
	want := "network.main_port\n  Main HTTP listener port.\n\nlogging.enabled\n  Whether logging is enabled.\n"
	if output.String() != want {
		t.Fatalf("compact output:\n%s\nwant:\n%s", output.String(), want)
	}
	if strings.Contains(output.String(), "key:") || strings.Contains(output.String(), "description:") || strings.Contains(output.String(), "settings:") {
		t.Fatalf("compact output contains field labels: %s", output.String())
	}

	output.Reset()
	detailed := map[string]any{"settings": []any{map[string]any{
		"active_value":     float64(8080),
		"source":           "default",
		"description":      "Main HTTP listener port.",
		"key":              "network.main_port",
		"storage":          "node",
		"configured_value": float64(8080),
		"default_value":    float64(8080),
	}}}
	renderValue(&output, detailed, 0)
	text := output.String()
	positions := []int{
		strings.Index(text, "key:"),
		strings.Index(text, "description:"),
		strings.Index(text, "storage:"),
		strings.Index(text, "configured_value:"),
		strings.Index(text, "active_value:"),
		strings.Index(text, "default_value:"),
	}
	for index, position := range positions {
		if position < 0 || index > 0 && position <= positions[index-1] {
			t.Fatalf("detailed fields are not ordered: %s", text)
		}
	}
}

func TestFlatResourceSummariesRenderAsReadableBlocks(t *testing.T) {
	value := map[string]any{"workers": []any{
		map[string]any{
			"sandbox_id": "sandbox-1", "owner_id": "orders", "worker_id": "worker-1", "state": "READY",
			"workload_type": "service", "workload_id": "core/example/orders", "in_flight": float64(0),
		},
		map[string]any{
			"sandbox_id": "sandbox-2", "owner_id": "reports", "worker_id": "worker-2", "state": "BUSY",
			"workload_type": "job", "workload_id": "daily-report", "in_flight": float64(1),
		},
	}}
	var output bytes.Buffer
	renderValue(&output, value, 0)
	want := "worker_id: worker-1\n" +
		"workload_type: service\n" +
		"state: READY\n" +
		"workload_id: core/example/orders\n" +
		"owner_id: orders\n" +
		"sandbox_id: sandbox-1\n" +
		"in_flight: 0\n\n" +
		"worker_id: worker-2\n" +
		"workload_type: job\n" +
		"state: BUSY\n" +
		"workload_id: daily-report\n" +
		"owner_id: reports\n" +
		"sandbox_id: sandbox-2\n" +
		"in_flight: 1\n"
	if output.String() != want {
		t.Fatalf("resource summary output:\n%s\nwant:\n%s", output.String(), want)
	}
	if strings.Contains(output.String(), "workers:") || strings.Contains(output.String(), "  -") {
		t.Fatalf("resource summary retained collection noise: %s", output.String())
	}

	output.Reset()
	renderValue(&output, map[string]any{"workers": []any{}}, 0)
	if output.String() != "workers: none\n" {
		t.Fatalf("empty resource summary = %q", output.String())
	}
}

func TestRenderingStructuredValuesWithScalarArraysTerminates(t *testing.T) {
	value := struct {
		Ready       bool            `json:"ready"`
		Controllers []string        `json:"controllers"`
		Plugins     map[string]bool `json:"plugins"`
	}{Ready: false, Controllers: []string{"cpu", "memory"}, Plugins: map[string]bool{"bridge": true}}
	var output bytes.Buffer
	renderValue(&output, map[string]any{"report": value}, 0)
	for _, expected := range []string{"ready: false", "- cpu", "- memory", "bridge: true"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestConciseExecutionRenderingIsOrderedAndKeepsExactIntegers(t *testing.T) {
	value := core.Result{
		"duration":     "432.032417ms",
		"state":        "SUCCEEDED",
		"resources":    map[string]any{"cpu_usage_micros": json.Number("15342046713")},
		"execution_id": "execution-1",
		"result":       map[string]any{"message": "Hello from Deno", "version": "2.9.4"},
	}
	var output bytes.Buffer
	renderValue(&output, value, 0)
	want := "state: SUCCEEDED\n" +
		"result:\n" +
		"  message: Hello from Deno\n" +
		"  version: 2.9.4\n" +
		"duration: 432.032417ms\n" +
		"resources:\n" +
		"  cpu_usage_micros: 15342046713\n" +
		"execution_id: execution-1\n"
	if output.String() != want {
		t.Fatalf("concise execution output:\n%s\nwant:\n%s", output.String(), want)
	}
	if strings.Contains(output.String(), "e+") {
		t.Fatalf("concise execution output used scientific notation: %s", output.String())
	}
}
