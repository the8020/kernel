# Purpose

- Implement `kernel.config.get` as declared by the adjacent authoritative TOML.

# Ownership

- Own typed key extraction, service delegation, and unknown-setting error mapping.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- Accept only node-local settings. The complete result includes the declared
  storage; callers never select a store.

# Work Guidance

- Do not duplicate settings state calculations.

# Verification

- Application integration validates storage, sources, and complete setting
  values.

# Child DOX Index
