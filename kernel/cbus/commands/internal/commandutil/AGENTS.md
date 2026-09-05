Parent DOX: [kernel/kernel/cbus/commands DOX](../../AGENTS.md).

# Purpose

- Share small typed extraction, JSON/CSV conversion, runtime-availability,
  command-error, and concise result helpers across runtime handlers.

# Ownership

- Convert already command-bus-validated primitive arguments into domain values
  and expose safe runtime failures from one synchronized runtime snapshot.
- Do not implement domain behavior, command discovery, transport parsing, or
  service lookup beyond the typed `RuntimeServices` field.

# Local Contracts

- Public API is internal to `kernel/cbus/commands`: `Runtime`, primitive
  accessors, `JSON`, `CSV`, `Duration`, `Permissions`, `AdministrativeExecution`
  and `OperationError`.
- Administrative eval/run responses share one concise default shape; the
  explicit detail view preserves the complete artifact, execution, and resource
  record.
- User-facing runtime errors retain stable command-bus codes. Structured
  execution failures cross the supervisor boundary without transport text; Deno
  application argument errors retain their code without parsing error text, and
  no operation silently invokes a fallback runtime.

# Work Guidance

- Keep handlers responsible for command-specific validation and result shape.

# Verification

- Unit tests cover domain-to-command error classification; command handler tests
  and generated-registry compilation cover result and argument helpers.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
