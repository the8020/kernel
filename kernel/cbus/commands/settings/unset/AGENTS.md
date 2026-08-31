# Purpose

- Implement `settings.unset` as declared by the adjacent authoritative TOML.

# Ownership

- Own typed key extraction, unset transaction delegation, and shared mutation error mapping.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- The setting definition selects the store from which the override is removed.

# Work Guidance

- Reuse `settings.Manager.Unset` and the set package's stable error mapping.

# Verification

- Application integration validates node/global removal and restoration of the
  next lower-precedence value.

# Child DOX Index
