Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Adapt shared topology primitives for package runtime operations.

# Ownership

- Own capacity-aware node listing and validated topology upsert/removal.
- Do not publish CBus metadata; `the8020/system` owns visible `system.nodes.*`
  command programs.

# Local Contracts

- Handlers delegate to the database-backed `kernel/nodes` owner. There are no
  command-bus operations for changing the fixed instance layout.
- A local recipient address or port change is effective after kernel restart.
- The private list adapter combines shared topology with bounded local/remote
  capacity reports; an unreachable or disabled node remains visible with its
  error.

# Work Guidance

# Verification

- Command generation and aggregate command tests verify the catalog and
  delegation.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
