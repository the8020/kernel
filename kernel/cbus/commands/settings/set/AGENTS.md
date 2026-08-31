# Purpose

- Implement `settings.set` as declared by the adjacent authoritative TOML.

# Ownership

- Own typed argument extraction, settings transaction delegation, result wrapping, and domain-to-command error mapping shared with unset.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler` and `MapError` for the sibling unset handler.
- The setting definition, never a command argument, selects node or global
  persistence. Network binding, logging preparation, validation, and
  persistence remain in their owners.

# Work Guidance

- Keep mapping limited to stable Phase 1 error codes.

# Verification

- Application integration covers successful node/global writes through the same
  command, network/logging mutation, and occupied-port rollback.

# Child DOX Index
