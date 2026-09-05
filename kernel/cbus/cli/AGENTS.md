Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Share all metadata-driven command UI behavior across administrative modes.

# Ownership

- Own live-catalog matching, kernel-command argument conversion, secure-input
  extraction, help, local errors, exit mapping, text/JSON rendering, and
  interactive line tokenization.
- Do not own console lifecycle, socket paths, transport implementation, or
  command-specific branches.

# Local Contracts

- Public API: `Executor`, `CatalogProvider`, `SecretResolver`, `Runner`, `New`,
  `NewDynamic`, `Runner.SetSecretResolver`, `Runner.Run`, `Runner.Help`, and
  `SplitLine`.
- Dynamic runners conditionally fetch the live catalog before every command or
  help action. Unknown/stale lookup refreshes once; stale execution refreshes
  and retries once with the same request ID.
- Package paths are one dotted visible token and every remaining token,
  including options and `--`, is forwarded unchanged. Kernel command argv is
  parsed by the server-side core adapter.
- Lookup and help use only each command's canonical visible path.
- Metadata-declared secure input is removed only through its explicit stdin
  option or obtained through the client resolver. It is transported separately
  and never placed in argv or history.
- `help` is local; its global view lists catalog commands plus the local `help`
  and interactive `exit` commands as normal command rows, and both local
  commands have detailed help topics.
- Catalog-matched operations use the typed executor; `admin` owns execution of
  the local `exit` command.
- A single-result array of `{key, description}` summaries renders compactly as
  unlabeled key and indented description rows.
- Every other single-result array of flat scalar summaries renders as unlabeled
  resource blocks separated by one blank line, with the resource ID first and
  domain fields in stable readable order; an empty collection renders as
  `<collection>: none`.
- Detailed objects render identity, description, declared settings storage,
  canonical location, configured/active/default values, overrides, source, and
  runtime state in stable preferred order before alphabetically ordered unknown
  fields.
- Named `core.Result` maps render directly instead of passing through lossy JSON
  normalization; execution summaries render state, result, logs, duration,
  resources, and execution identity in that order when present.
- Text-mode command failures render their stable code and message followed by
  any structured error details; JSON mode retains the complete response
  envelope.
- Extend syntax only through command metadata or shared client-local behavior.

# Work Guidance

- Keep one parser and renderer for one-shot and interactive modes.

# Verification

- `cli_test.go` covers typed kernel arguments, raw package argv, dynamic/stale
  catalog refresh, secure input, complete global help, catalog help, examples,
  local and structured command errors, compact setting and flat resource
  summaries, empty collections, detailed rendering order, exact large-integer
  rendering, and quoted input.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
