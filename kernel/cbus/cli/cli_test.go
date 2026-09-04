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

type recordingCatalog struct {
	calls    []string
	catalogs []core.Catalog
}

func (c *recordingCatalog) Catalog(_ context.Context, known string) (core.Catalog, bool, error) {
	c.calls = append(c.calls, known)
	if len(c.catalogs) == 0 {
		return core.Catalog{}, true, nil
	}
	next := c.catalogs[0]
	if len(c.catalogs) > 1 {
		c.catalogs = c.catalogs[1:]
	}
	return next, known != "" && known == next.Revision, nil
}

type sequenceExecutor struct {
	requests  []core.Request
	responses []core.Response
}

func (e *sequenceExecutor) Execute(_ context.Context, request core.Request) (core.Response, error) {
	e.requests = append(e.requests, request)
	response := e.responses[0]
	if len(e.responses) > 1 {
		e.responses = e.responses[1:]
	}
	return response, nil
}

func testCatalog() []core.Command {
	return []core.Command{{ID: "thing.set", Path: []string{"thing", "set"}, Summary: "Set thing", Description: "Sets a thing.", Parameters: []core.Parameter{{Name: "name", Type: "string", Required: true}, {Name: "count", Type: "integer", Position: 1, Required: true}, {Name: "enabled", Type: "boolean", Position: 2, Required: true}, {Name: "namespace", Type: "string", Option: "namespace", Description: "Grouping namespace."}, {Name: "detached", Type: "boolean", Option: "detached", Description: "Return without waiting."}}, Examples: []string{"thing set x 2 true --namespace demo --detached"}}}
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
	if code := runner.Run(context.Background(), []string{"thing", "set", "value", "--namespace=demo", "2", "--detached", "true"}, false, &output); code != 0 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	if executor.request.CommandID != "thing.set" || strings.Join(executor.request.Argv, "|") != "value|--namespace=demo|2|--detached|true" || executor.request.Arguments != nil {
		t.Fatalf("request: %#v", executor.request)
	}
	if !strings.Contains(output.String(), "ok: true") {
		t.Fatalf("output: %s", output.String())
	}
	help := runner.Help([]string{"thing", "set"})
	for _, expected := range []string{"Usage: thing set <name> <count> <enabled> [--detached] [--namespace <namespace>]", "Options:", "--detached", "thing set x 2 true"} {
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

func TestDynamicRunnerConditionallyRefreshesItsCatalog(t *testing.T) {
	command := core.Command{ID: "example/tool/run@one", Name: "example.tool.run", Path: []string{"example.tool.run"}, Summary: "Run"}
	provider := &recordingCatalog{catalogs: []core.Catalog{
		{ProtocolVersion: core.ProtocolVersion, Revision: "one", Commands: []core.Command{command}},
		{ProtocolVersion: core.ProtocolVersion, Revision: "one", Commands: []core.Command{command}},
	}}
	executor := &fakeExecutor{}
	runner := NewDynamic(provider, executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"example.tool.run", "first"}, false, &output); code != 0 {
		t.Fatalf("first run = %d: %s", code, output.String())
	}
	output.Reset()
	if code := runner.Run(context.Background(), []string{"example.tool.run", "second"}, false, &output); code != 0 {
		t.Fatalf("second run = %d: %s", code, output.String())
	}
	if len(provider.calls) != 2 || provider.calls[0] != "" || provider.calls[1] != "one" {
		t.Fatalf("catalog conditions = %#v", provider.calls)
	}
}

func TestDynamicRunnerRefreshesAndRetriesOneStaleExecution(t *testing.T) {
	oldCommand := core.Command{ID: "example/tool/run@old", Name: "example.tool.run", Path: []string{"example.tool.run"}, Summary: "Run"}
	newCommand := oldCommand
	newCommand.ID = "example/tool/run@new"
	provider := &recordingCatalog{catalogs: []core.Catalog{
		{ProtocolVersion: core.ProtocolVersion, Revision: "old", Commands: []core.Command{oldCommand}},
		{ProtocolVersion: core.ProtocolVersion, Revision: "new", Commands: []core.Command{newCommand}},
	}}
	executor := &sequenceExecutor{responses: []core.Response{
		{ProtocolVersion: core.ProtocolVersion, CatalogRevision: "new", Error: core.NewError(core.CodeStaleCatalog, "changed")},
		{ProtocolVersion: core.ProtocolVersion, CatalogRevision: "new", Success: true, Result: core.Result{"ok": true}},
	}}
	runner := NewDynamic(provider, executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"example.tool.run", "--untouched", "value"}, false, &output); code != 0 {
		t.Fatalf("retry run = %d: %s", code, output.String())
	}
	if len(provider.calls) != 2 || provider.calls[1] != "" {
		t.Fatalf("catalog calls = %#v", provider.calls)
	}
	if len(executor.requests) != 2 {
		t.Fatalf("execution requests = %#v", executor.requests)
	}
	first, second := executor.requests[0], executor.requests[1]
	if first.CommandID != oldCommand.ID || first.CatalogRevision != "old" || second.CommandID != newCommand.ID || second.CatalogRevision != "new" {
		t.Fatalf("retry requests = %#v", executor.requests)
	}
	if first.RequestID == "" || second.RequestID != first.RequestID || strings.Join(second.Argv, "|") != "--untouched|value" {
		t.Fatalf("retry identity/argv = %#v", executor.requests)
	}
}

func TestArgumentsRemainRawAndLineParsingReportsQuotes(t *testing.T) {
	executor := &fakeExecutor{}
	runner := New(testCatalog(), executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"thing", "set", "x", "no", "true"}, false, &output); code != 0 {
		t.Fatalf("code %d output %s", code, output.String())
	}
	if strings.Join(executor.request.Argv, "|") != "x|no|true" {
		t.Fatalf("raw argv = %#v", executor.request.Argv)
	}
	if _, err := SplitLine("'unfinished"); err == nil {
		t.Fatal("accepted unfinished quote")
	}
	tokens, err := SplitLine(`thing set "hello world" 2 true`)
	if err != nil || tokens[2] != "hello world" {
		t.Fatalf("tokens %#v error %v", tokens, err)
	}
}

func TestOptionsAndTerminatorRemainRaw(t *testing.T) {
	executor := &fakeExecutor{}
	runner := New([]core.Command{{ID: "test.run", Path: []string{"test", "run"}, Summary: "test", Description: "test"}}, executor)
	var output bytes.Buffer
	if code := runner.Run(context.Background(), []string{"test", "run", "--missing", "--", "--literal"}, false, &output); code != 0 || strings.Join(executor.request.Argv, "|") != "--missing|--|--literal" {
		t.Fatalf("terminator: code %d request %#v output %q", code, executor.request, output.String())
	}
}

func TestMetadataDeclaredSecretInput(t *testing.T) {
	command := core.Command{
		ID: "user.add", Path: []string{"user", "add"}, Summary: "add", Description: "add",
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
	if code := runner.Run(context.Background(), []string{"user", "add", "admin"}, false, &output); code != 0 || fromStdin {
		t.Fatalf("prompt run code=%d fromStdin=%t output=%q", code, fromStdin, output.String())
	}
	if executor.request.Secrets["password"] != "not-in-command-history" || strings.Join(executor.request.Argv, "|") != "admin" {
		t.Fatalf("resolved request = %#v", executor.request)
	}
	if help := runner.Help(command.Path); !strings.Contains(help, "[--password-stdin]") || strings.Contains(help, "<password>") {
		t.Fatalf("secret help exposed an ordinary password argument:\n%s", help)
	}

	output.Reset()
	if code := runner.Run(context.Background(), []string{"user", "add", "admin", "--password-stdin"}, false, &output); code != 0 || !fromStdin {
		t.Fatalf("stdin run code=%d fromStdin=%t output=%q", code, fromStdin, output.String())
	}
	output.Reset()
	if code := runner.Run(context.Background(), []string{"user", "add", "admin", "ordinary-password"}, false, &output); code != 0 || strings.Join(executor.request.Argv, "|") != "admin|ordinary-password" {
		t.Fatalf("ordinary password run code=%d output=%q", code, output.String())
	}
}

func TestOnlyMetadataDeclaredSecretsArePrompted(t *testing.T) {
	command := core.Command{
		ID: "user.add", Path: []string{"user", "add"}, Summary: "add", Description: "add",
		Parameters: []core.Parameter{
			{Name: "username", Type: "string", Position: 0, Required: true, Prompt: "Username: "},
			{Name: "password", Type: "string", Required: true, Secret: true, SecretPrompt: "Password: ", SecretConfirmationPrompt: "Confirm password: ", SecretStdinOption: "password-stdin"},
		},
	}
	executor := &fakeExecutor{}
	runner := New([]core.Command{command}, executor)
	var events []string
	runner.SetSecretResolver(func(prompt, _ string, _ bool) (string, error) {
		events = append(events, prompt)
		return "secure-password", nil
	})
	var output bytes.Buffer
	if code := runner.Run(context.Background(), command.Path, false, &output); code != 0 {
		t.Fatalf("prompted run code=%d output=%q", code, output.String())
	}
	if strings.Join(events, "|") != "Password: " {
		t.Fatalf("prompt order = %#v", events)
	}
	if len(executor.request.Argv) != 0 || executor.request.Secrets["password"] != "secure-password" {
		t.Fatalf("prompted request = %#v", executor.request.Arguments)
	}

	events = nil
	output.Reset()
	if code := runner.Run(context.Background(), append(append([]string(nil), command.Path...), "explicit-admin"), false, &output); code != 0 {
		t.Fatalf("explicit run code=%d output=%q", code, output.String())
	}
	if strings.Join(events, "|") != "Password: " || strings.Join(executor.request.Argv, "|") != "explicit-admin" {
		t.Fatalf("explicit prompt events=%#v request=%#v", events, executor.request.Arguments)
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
