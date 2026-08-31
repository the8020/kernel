# Purpose

- Provide the single administrative executable's one-shot and interactive modes.

# Ownership

- Own global client options, console input, terminal line editing, session command history, prompts, local `help`/`exit`, and output coordination.
- Never call settings, network, logging, lifecycle, or other kernel services directly; all operations use the typed command-bus client.

# Local Contracts

- Public API: `Main`.
- Both modes share the generated catalog and `cbus/cli` runner.
- Default targeting is the exact canonical current directory; `--root` is the only override and `--json` preserves structured responses.
- Global `help` lists every catalog command together with local `help` and `exit` commands; local commands must not be relegated to footer text.
- Interactive TTY editing delegates raw mode, terminal-width-aware wrapped rows, cursor movement, editing keys, and bracketed paste to the maintained `golang.org/x/term` terminal implementation. Do not maintain a project-owned escape parser or cursor renderer.
- While terminal raw mode is active, route all interactive command results and errors through the `golang.org/x/term` writer so every newline returns to column zero; never write command output directly to the raw TTY.
- Session history retains the latest 100 non-empty commands and suppresses consecutive duplicates. Bracketed CRLF/multiline paste is joined into one command before dispatch and renders only the initial primary prompt while the paste is collected.
- Metadata-declared passwords use the terminal library's no-echo password reader with confirmation and never enter command history. Redirected automation is accepted only through the command's explicit standard-input flag.
- Metadata-declared ordinary prompts acquire omitted required positional values
  in terminal one-shot and interactive modes before password input. Prompted
  values do not become standalone interactive history entries; redirected
  one-shot use must supply them as command tokens.
- Terminal mode is used only when both input and output are terminals, receives the detected terminal dimensions, and must always be restored on exit. Redirected and scripted input keeps the scanner path without terminal escape output.
- Raw-terminal `Ctrl-C` exits the interactive client immediately; when `run.sh` owns the kernel, the restored terminal then receives that wrapper's live graceful-shutdown progress.
- Lifecycle commands never exit the interactive client. The prompt remains
  available across kernel self-restart and after shutdown until the user invokes
  local `exit` or sends `Ctrl-C`.
- Extend only with client-local behavior or transport options required by an active phase.

# Work Guidance

- Do not add command-specific client branches or reimplement terminal key parsing, wrapped-line layout, or repaint behavior.

# Verification

- `line_editor_test.go` verifies library-backed cursor editing, deletion, narrow
  wrapped-row history movement without prompt duplication, history bounds,
  metadata-prompted value acquisition without history pollution, single-prompt
  bracketed multiline paste, CRLF input handling, raw-terminal CRLF output
  alignment, the non-terminal scanner fallback, and continued console input
  after a lifecycle command.
- Console editing or output changes require a real narrow-width PTY smoke using wrapped input, bracketed multiline paste, history recall, left/right movement, and a live command response; in-memory byte assertions alone are insufficient.
- `app/integration_test.go` exercises one-shot and interactive use against the same live catalog and verifies that interactive help exposes `exit`.

# Child DOX Index
