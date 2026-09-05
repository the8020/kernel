Parent DOX: [kernel/kernel/cbus/commands/settings DOX](../AGENTS.md).

# Purpose

- Implement `kernel.config.unset` as declared by the adjacent authoritative
  TOML.

# Ownership

- Own typed key extraction, unset transaction delegation, and shared mutation
  error mapping.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- Accept only node-local settings and remove the override from node
  configuration.

# Work Guidance

- Reuse `settings.Manager.Unset` and the set package's stable error mapping.

# Verification

- Application integration validates node-local removal and restoration of the
  default value.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
