Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Expose Phase 1B host-owned port lease inventory, allocation, and closure.

# Ownership

- Own declarative port list/expose/close handlers.

# Local Contracts

- `port list` exposes only lease identity, protocol/state, host bind endpoint,
  sandbox/internal-port target, purpose, and optional expiration.
- Exposure resolves sandbox IP and selected-backend target port from kernel
  state while preserving the declared internal port; loopback remains the
  default bind and public binds remain policy-controlled.

# Work Guidance

- Never hand a host listener or containerd authority to Deno.

# Verification

- Generated validation and handler tests cover allocation options and closure.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.

- This domain contract owns its leaf command folders; they contain only one
  declarative command and thin handler each.
