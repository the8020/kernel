# Purpose

- Implement `kernel.shutdown` as declared by the adjacent authoritative TOML.

# Ownership

- Own only delegation to `lifecycle.Manager.Request` and acknowledgement.
- Do not stop services directly.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- There is no short alias. The idempotent command remains callable while the
  command server drains; progress is read through `kernel.status`
  until the administrative socket closes.

# Work Guidance

- Preserve idempotent lifecycle delegation.

# Verification

- Application integration validates the response, orderly exit, socket/PID
  cleanup, lock release, and continued interactive-console loop.

# Child DOX Index
