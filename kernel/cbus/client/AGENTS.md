Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Send typed command-bus requests to one local Unix socket.

# Ownership

- Own Unix dialing, HTTP request encoding, response decoding, timeouts, request
  defaults, and idle connection closure.
- Do not parse command lines, render results, or call services.

# Local Contracts

- Public API: `Client`, `New`, `Catalog`, `Execute`, and `Close`.
- Catalogs, requests, and responses use `core` envelopes and protocol version 2.
  Conditional catalog reads preserve the last process-local revision.
- Response decoding preserves JSON numbers as `json.Number` so human output
  never changes large integers into scientific notation or loses integer
  precision.
- The five-minute client deadline exceeds bounded package-download and sandboxed
  schema-evaluation deadlines; do not reintroduce a shorter global timeout that
  cancels valid commands.
- Extend transport addressing only when a future accepted administration
  transport exists.

# Work Guidance

- Keep command behavior generic and metadata independent.

# Verification

- Server transport and application integration tests exercise the real client.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
