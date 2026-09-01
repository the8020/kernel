package admin

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"the8020/kernel/cbus/cli"
	"the8020/kernel/cbus/core"
)

type countingExecutor struct {
	calls   int
	request core.Request
}

func (e *countingExecutor) Execute(_ context.Context, request core.Request) (core.Response, error) {
	e.calls++
	e.request = request
	return core.Response{ProtocolVersion: core.ProtocolVersion, RequestID: request.RequestID, Success: true, Result: core.Result{"requested": true}}, nil
}

func TestTerminalEditorMovesCursorAndInsertsText(t *testing.T) {
	reader, _ := newTestTerminalReader("abc\x1b[D\x1b[DX\r", 80)
	line := readTerminalLine(t, reader)
	if line != "aXbc" {
		t.Fatalf("line = %q, want %q", line, "aXbc")
	}
}

func TestTerminalEditorSupportsHomeEndDeleteAndBackspace(t *testing.T) {
	reader, _ := newTestTerminalReader("abcd\x1b[H\x1b[C\x1b[3~\x1b[F\x7f\r", 80)
	line := readTerminalLine(t, reader)
	if line != "ac" {
		t.Fatalf("line = %q, want %q", line, "ac")
	}
}

func TestTerminalEditorRecallsWrappedHistoryWithoutDuplicatingPrompt(t *testing.T) {
	command := `runtime eval 'export default () => ({ message: "Hello from Deno", version: Deno.version.deno })'`
	input := command + "\r" + "\x1b[A" + strings.Repeat("\x1b[D", 32) + strings.Repeat("\x1b[C", 16) + "\r"
	reader, output := newTestTerminalReader(input, 28)
	if first := readTerminalLine(t, reader); first != command {
		t.Fatalf("first line = %q, want %q", first, command)
	}
	if second := readTerminalLine(t, reader); second != command {
		t.Fatalf("recalled line = %q, want %q", second, command)
	}
	if prompts := strings.Count(output.String(), "admin> "); prompts != 2 {
		t.Fatalf("prompt rendered %d times for two wrapped lines; output length = %d", prompts, output.Len())
	}
}

func TestTerminalEditorTreatsBracketedMultilinePasteAsOneCommand(t *testing.T) {
	pasted := "runtime eval 'export default () => ({\r\n" +
		"message: \"Hello from Deno\",\r\n" +
		"version: Deno.version.deno\r\n" +
		"})'\r\n"
	want := `runtime eval 'export default () => ({ message: "Hello from Deno", version: Deno.version.deno })'`
	reader, output := newTestTerminalReader("\x1b[200~"+pasted+"\x1b[201~\r", 40)
	line := readTerminalLine(t, reader)
	if line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	args, err := cli.SplitLine(line)
	if err != nil {
		t.Fatalf("SplitLine() error = %v", err)
	}
	if len(args) != 3 || args[0] != "runtime" || args[1] != "eval" {
		t.Fatalf("args = %#v, want one runtime eval command", args)
	}
	if reader.history.Len() != 1 || reader.history.At(0) != want {
		t.Fatalf("history = %#v, want one joined paste entry", reader.history.entries)
	}
	if prompts := strings.Count(output.String(), "admin> "); prompts != 1 {
		t.Fatalf("multiline paste rendered %d prompts, want one; output = %q", prompts, output.String())
	}
}

func TestTerminalEditorConsumesPastedCRLFAsOneLineEnding(t *testing.T) {
	reader, _ := newTestTerminalReader("first\r\nsecond\r", 80)
	first := readTerminalLine(t, reader)
	second := readTerminalLine(t, reader)
	if first != "first" || second != "second" {
		t.Fatalf("lines = %q, %q; want %q, %q", first, second, "first", "second")
	}
}

func TestSessionHistoryBoundsAndDeduplicates(t *testing.T) {
	history := &sessionHistory{}
	for index := 0; index <= maxCommandHistory; index++ {
		history.Add(fmt.Sprintf("command-%d", index))
		history.finalizeLatest()
	}
	history.Add(history.At(0))
	history.finalizeLatest()
	if history.Len() != maxCommandHistory {
		t.Fatalf("history length = %d, want %d", history.Len(), maxCommandHistory)
	}
	if history.At(maxCommandHistory-1) != "command-1" {
		t.Fatalf("oldest history entry = %q, want the first entry evicted", history.At(maxCommandHistory-1))
	}
}

func TestInteractiveLineReaderKeepsScannerFallback(t *testing.T) {
	var output bytes.Buffer
	reader := newInteractiveLineReader(strings.NewReader("status\n"), &output)
	line, ok, err := reader.ReadLine("admin> ")
	if err != nil {
		t.Fatalf("ReadLine() error = %v", err)
	}
	if !ok || line != "status" {
		t.Fatalf("ReadLine() = %q, %v; want %q, true", line, ok, "status")
	}
	if output.String() != "admin> " {
		t.Fatalf("output = %q, want prompt only", output.String())
	}
}

func TestSecretReaderUsesNoEchoConfirmationAndExplicitStdin(t *testing.T) {
	terminalReader, terminalOutput := newTestTerminalReader("correct horse battery staple\rcorrect horse battery staple\r", 80)
	secret, err := terminalReader.ReadSecret("Password: ", "Confirm password: ", false)
	if err != nil || secret != "correct horse battery staple" {
		t.Fatalf("terminal secret = %q, error = %v", secret, err)
	}
	if strings.Contains(terminalOutput.String(), secret) {
		t.Fatalf("terminal echoed secret: %q", terminalOutput.String())
	}

	var output bytes.Buffer
	scripted := newInteractiveLineReader(strings.NewReader("automation-password\n"), &output)
	secret, err = scripted.ReadSecret("Password: ", "Confirm password: ", true)
	if err != nil || secret != "automation-password" || output.Len() != 0 {
		t.Fatalf("stdin secret = %q, error = %v, output = %q", secret, err, output.String())
	}
	if _, err := newInteractiveLineReader(strings.NewReader("password\n"), &output).ReadSecret("Password: ", "Confirm password: ", false); err == nil || !strings.Contains(err.Error(), "standard-input option") {
		t.Fatalf("non-terminal prompt error = %v", err)
	}
}

func TestPromptedValueDoesNotReplaceCommandHistory(t *testing.T) {
	command := "auth bootstrap-admin add"
	reader, output := newTestTerminalReader(command+"\rprompted-admin\r", 80)
	if line := readTerminalLine(t, reader); line != command {
		t.Fatalf("command = %q, want %q", line, command)
	}
	reader.AddHistory(command)
	value, err := reader.ReadValue("Username: ")
	if err != nil || value != "prompted-admin" {
		t.Fatalf("prompted value = %q, error = %v", value, err)
	}
	if reader.history.Len() != 1 || reader.history.At(0) != command {
		t.Fatalf("history = %#v, want only %q", reader.history.entries, command)
	}
	if !strings.Contains(output.String(), "Username: ") {
		t.Fatalf("prompt output = %q", output.String())
	}
}

func TestTerminalWriterReturnsEveryStructuredLineToColumnZero(t *testing.T) {
	reader, output := newTestTerminalReader("status\r", 120)
	_ = readTerminalLine(t, reader)
	output.Reset()

	_, err := fmt.Fprint(reader.Writer(output), "execution:\n  artifact_id: artifact-1\n  execution:\n    state: SUCCEEDED\nresources:\n  pid_current: 7\n")
	if err != nil {
		t.Fatalf("write structured response: %v", err)
	}

	want := "execution:\r\n  artifact_id: artifact-1\r\n  execution:\r\n    state: SUCCEEDED\r\nresources:\r\n  pid_current: 7\r\n"
	if output.String() != want {
		t.Fatalf("terminal output = %q, want %q", output.String(), want)
	}
}

func TestScannerWriterPreservesSeparateOutputStreams(t *testing.T) {
	var promptOutput bytes.Buffer
	var errorOutput bytes.Buffer
	reader := newInteractiveLineReader(strings.NewReader(""), &promptOutput)

	_, _ = fmt.Fprint(reader.Writer(&errorOutput), "error: failed\n")
	if promptOutput.Len() != 0 || errorOutput.String() != "error: failed\n" {
		t.Fatalf("prompt output = %q, error output = %q", promptOutput.String(), errorOutput.String())
	}
}

func TestInteractiveConsoleStaysOpenAfterSuccessfulKernelShutdown(t *testing.T) {
	executor := &countingExecutor{}
	runner := cli.New([]core.Command{
		{ID: "system.shutdown", Path: []string{"system", "shutdown"}, Summary: "shutdown", Description: "shutdown", RestartBehavior: "stops_kernel"},
		{ID: "system.status", Path: []string{"system", "status"}, Summary: "status", Description: "status"},
	}, executor)
	var output bytes.Buffer
	code := interactive(context.Background(), runner, strings.NewReader("system shutdown\nsystem status\n"), &output, &output, false)
	if code != 0 || executor.calls != 2 || strings.Count(output.String(), "requested: true") != 2 {
		t.Fatalf("code=%d calls=%d output=%q", code, executor.calls, output.String())
	}
}

func TestInteractiveConsoleResolvesMetadataPromptedValue(t *testing.T) {
	executor := &countingExecutor{}
	runner := cli.New([]core.Command{{
		ID: "test.add", Path: []string{"test", "add"}, Summary: "add", Description: "add",
		Parameters: []core.Parameter{{Name: "username", Type: "string", Required: true, Prompt: "Username: "}},
	}}, executor)
	var output bytes.Buffer
	code := interactive(context.Background(), runner, strings.NewReader("test add\nprompted-admin\n"), &output, &output, false)
	if code != 0 || executor.calls != 1 || executor.request.Arguments["username"] != "prompted-admin" {
		t.Fatalf("code=%d calls=%d request=%#v output=%q", code, executor.calls, executor.request, output.String())
	}
	if !strings.Contains(output.String(), "Username: ") {
		t.Fatalf("output omitted username prompt: %q", output.String())
	}
}

func newTestTerminalReader(input string, width int) (*interactiveLineReader, *bytes.Buffer) {
	output := &bytes.Buffer{}
	return newTerminalLineReader(strings.NewReader(input), output, width, 24), output
}

func readTerminalLine(t *testing.T, reader *interactiveLineReader) string {
	t.Helper()
	line, ok, err := reader.ReadLine("admin> ")
	if err != nil {
		t.Fatalf("ReadLine() error = %v", err)
	}
	if !ok {
		t.Fatal("ReadLine() unexpectedly reached EOF")
	}
	return line
}
