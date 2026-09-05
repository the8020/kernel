Parent DOX: [kernel/kernel/runtime DOX](../AGENTS.md).

# Purpose

- Expose the generated Go runtime-protocol envelope and message types to
  authored kernel packages.

# Ownership

- Own only `generated.go`, which is a byte-identical generated mirror of
  `.development/generated/runtime/protocol.go`.
- Do not own schema decisions, payload models, transport, authentication, or
  runtime behavior.

# Local Contracts

- `defaults/config/runtime/protocol/schema.json` is authoritative;
  `go run ./kernel/cbus/gen` is the only writer.
- Every envelope validates protocol version, generated message type, and
  runtime-group identity; correlation IDs are enforced by the applicable
  consumer.

# Work Guidance

- Never edit `generated.go` manually.

# Verification

- Generator tests prove both Go protocol outputs are byte-identical and cover
  every schema message.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
