// Browser half of the headless-to-renderer state qualification experiment.
// deno-lint-ignore-file no-explicit-any
import { Terminal } from "npm:@xterm/xterm@5.5.0";
import * as state from "./xterm_state.ts";
const { captureTerminal, restoreTerminal } = state;

const cases = (globalThis as any).terminalCases as any[];
const results = [];
for (const test of cases) {
  const terminal = new Terminal({ allowProposedApi: true });
  const host = document.createElement("div");
  document.body.append(host);
  terminal.open(host);
  const replies: string[] = [];
  terminal.onData((data) => replies.push(data));
  try {
    restoreTerminal(terminal, test.before);
    await new Promise<void>((resolve) => terminal.write(
      typeof test.after === "string" ? test.after : new Uint8Array(test.after), resolve));
    const actual = captureTerminal(terminal);
    const equal = JSON.stringify(actual) === JSON.stringify(test.expected) &&
      JSON.stringify(replies) === JSON.stringify(test.expectedReplies);
    results.push({ name: test.name, equal, ...(!equal ? { actual, expected: test.expected, replies, expectedReplies: test.expectedReplies } : {}) });
  } catch (error) {
    results.push({ name: test.name, equal: false, error: String(error) });
  } finally {
    terminal.dispose();
    host.remove();
  }
}
const output = document.createElement("pre");
// The package's view role must suppress parser replies while preserving actual
// xterm keyboard/paste encoding. The older isolated prototype has no view role.
if ("installTerminalView" in state) {
  for (const test of cases) {
    const terminal = new Terminal({ allowProposedApi: true });
    const host = document.createElement("div");
    document.body.append(host);
    terminal.open(host);
    const replies: string[] = [];
    terminal.onData((data) => replies.push(data));
    try {
      (state as any).installTerminalView(terminal);
      restoreTerminal(terminal, test.before);
      await new Promise<void>((resolve) => terminal.write(typeof test.after === "string" ? test.after : new Uint8Array(test.after), resolve));
      const equal = JSON.stringify(captureTerminal(terminal)) === JSON.stringify(test.expected) && replies.length === 0;
      terminal.paste("paste text");
      const paste = terminal.modes.bracketedPasteMode ? "\x1b[200~paste text\x1b[201~" : "paste text";
      results.push({ name: `view: ${test.name}`, equal: equal && replies.length === 1 && replies[0] === paste });
    } catch (error) { results.push({ name: `view: ${test.name}`, equal: false, error: String(error) }); }
    finally { terminal.dispose(); host.remove(); }
  }
}
output.id = "result";
output.textContent = JSON.stringify({ stateVersion: cases[0]?.before.version, equal: results.filter((result) => result.equal).length, total: results.length, results });
document.body.append(output);
