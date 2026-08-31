# Purpose

- Expose shared application-server topology administration through the command bus.

# Ownership

- Own capacity-aware node listing, validated topology upsert/removal, and
  initialized shared-root mapping get/set handlers and definitions.

# Local Contracts

- Handlers delegate to `kernel/nodes`; they never edit `config/nodes.toml` directly.
- `node paths get` returns the canonical `packages`, `config`, `state`, and
  `users` roots. `node paths set` validates all four roots and required Unix
  metadata semantics, records them atomically in `node/kernel/paths.toml`, and
  takes effect after restart.
- A local recipient address or port change is effective after kernel restart.
- `node.list` combines shared topology with bounded local/remote capacity
  reports; an unreachable or disabled node remains visible with its error.

# Work Guidance

# Verification

- Command generation and aggregate command tests verify the catalog and delegation.

# Child DOX Index
