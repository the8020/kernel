# Purpose

- Share small typed extraction, JSON/CSV conversion, runtime-availability, command-error, and concise result helpers across runtime handlers.

# Ownership

- Convert already command-bus-validated primitive arguments into domain values and expose safe runtime failures from one synchronized runtime snapshot.
- Do not implement domain behavior, command discovery, transport parsing, or service lookup beyond the typed `RuntimeServices` field.

# Local Contracts

- Public API is internal to `kernel/cbus/commands`: `Runtime`, primitive accessors, `JSON`, `CSV`, `Duration`, `Permissions`, `AdministrativeExecution`, `WebServiceStatus`, and `OperationError`.
- Administrative eval/run responses share one concise default shape; the explicit detail view preserves the complete artifact, execution, and resource record.
- Filesystem service lifecycle responses share one concise generation/capacity
  shape; `--detail` preserves complete configuration, sandbox allocations,
  Workers, failures, and metrics.
- User-facing runtime errors retain stable command-bus codes and never silently invoke a fallback runtime.

# Work Guidance

- Keep handlers responsible for command-specific validation and result shape.

# Verification

- Command handler tests and generated-registry compilation cover these helpers.

# Child DOX Index
