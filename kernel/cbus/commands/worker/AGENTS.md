Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Expose generic Phase 1B Worker lookup and termination.

# Ownership

- Own declarative Worker list/inspect/stop/kill handlers and delegate through
  the shared Worker manager.

# Local Contracts

- `worker list` exposes only Worker ID, workload type, state, workload/owner
  identity, sandbox ID, in-flight count, and any failure; `worker inspect` owns
  the complete Worker record.
- Kill requests immediate supervisor termination; it does not implicitly kill a
  sandbox.

# Work Guidance

- Preserve capitalized Worker terminology in user-facing descriptions.

# Verification

- Generated validation and handler tests cover filters and both termination
  modes.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.

- This domain contract owns its leaf command folders; they contain only one
  declarative command and thin handler each.
