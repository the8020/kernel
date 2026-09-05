Parent DOX: [kernel/kernel/cbus/commands/development DOX](../AGENTS.md).

# Purpose

- Share thin command-bus conversion and result shaping across development
  command leaves.

# Ownership

- Own typed service lookup, sandbox result helpers, activation-option JSON and
  CSV conversion, and standard operation-error mapping.
- Do not own development behavior, persistence, Git, or command metadata.

# Local Contracts

- Every handler delegates exactly once to `services.Development`; activation
  domain results remain observable even when their final status is conflicted or
  failed.

# Work Guidance

- Keep conversions deterministic and reject malformed structured options before
  domain invocation.

# Verification

- `handlers_test.go` invokes all 14 development command handlers, proves one
  service delegation per operation, and checks structured activation conversion;
  generated-handler compilation verifies every leaf binding.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
