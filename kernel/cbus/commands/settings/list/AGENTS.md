Parent DOX: [kernel/kernel/cbus/commands/settings DOX](../AGENTS.md).

# Purpose

- Implement `kernel.config.list` as declared by the adjacent authoritative TOML.

# Ownership

- Own compact summary shaping and delegation returning detailed `settings.Info`
  records.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- Include only node-local settings. With no argument, return each key and
  description. With `detail`, return complete records including declared
  storage. Reject every other view.

# Work Guidance

- Keep ordering and information calculation in `kernel/settings`.

# Verification

- Command-bus integration tests verify compact, detailed, and invalid views.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
