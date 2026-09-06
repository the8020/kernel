Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Define the transport-independent version-2 command contract and immutable
  process-local registry.

# Ownership

- Own catalog/metadata types, request/response envelopes, stable error codes,
  kernel argument conversion, secure-input validation, immutable snapshot
  publication, IDs, and dispatch.
- Do not own sockets, CLI tokens, settings, or domain behavior.

# Local Contracts

- Public API is the exported protocol/catalog types, `NewError`, `NewRegistry`,
  registry methods, `NewRequestID`, and `PathString`.
- Core registration and complete package replacement publish new immutable
  snapshots atomically. Execution loads one snapshot and releases registry locks
  before invoking a handler.
- Package commands use raw argv and separate declared secure-input maps. Kernel
  commands support string/integer/boolean positionals and long options and are
  converted in the core adapter before typed handlers run.
- Each command has one canonical visible name/path; aliases are not part of the
  protocol.
- Unknown secure inputs and missing required secure inputs fail before dispatch.
  Unexpected errors become safe internal errors and are logged with request ID,
  never secure values.
- Package dispatch may return an allocated `ExecutionReference` alongside its
  result or error. The response preserves it in either case: canonical program,
  job/node/sandbox/Worker/context IDs, parent context, saved log position and
  invocation times. Failures before allocation omit it. Console messages never
  appear in command response arrays; logd owns their bounded query path.
- Request correlation IDs use shared `cor-` generation and propagate entropy
  failures. The registry owns a `cat-` incarnation plus a monotonic revision;
  the incarnation changes on registry creation, while revisions are counters,
  not additional resource IDs. Clients retain correlation across catalog retry.
- Extend protocol metadata only when command TOML generation and both clients
  can consume it generically.

# Work Guidance

- Preserve stable error code strings and never expose stack traces.

# Verification

- `core_test.go` covers typed conversion, secure-input validation, duplicate and
  reserved names, unknown commands, dispatch, and concurrent atomic swaps.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
