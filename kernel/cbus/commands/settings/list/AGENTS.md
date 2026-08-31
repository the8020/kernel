# Purpose

- Implement `settings.list` as declared by the adjacent authoritative TOML.

# Ownership

- Own compact summary shaping and delegation returning detailed `settings.Info` records.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- With no argument, return only each setting key and description. With `detail`,
  return complete setting records including declared storage. Reject every other
  view.

# Work Guidance

- Keep ordering and information calculation in `kernel/settings`.

# Verification

- Command-bus integration tests verify compact, detailed, and invalid views.

# Child DOX Index
