Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Expose Phase 1B Deno inspector targets and temporary loopback debug leases.

# Ownership

- Own declarative debug targets/open/close handlers.

# Local Contracts

- `debug targets` exposes concise target identity, type, title, execution
  identity, and description; connection URLs are returned only when opening a
  debug lease.
- Debug leases are explicit, bounded, token-ready loopback exposure; target
  discovery delegates to the debugging manager.

# Work Guidance

- Resolve sandbox state before invoking inspector operations.

# Verification

- Generated validation and handler tests cover all debug commands.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.

- This domain contract owns its leaf command folders; they contain only one
  declarative command and thin handler each.
