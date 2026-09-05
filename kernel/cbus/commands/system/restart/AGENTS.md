Parent DOX: [kernel/kernel/cbus/commands/system DOX](../AGENTS.md).

# Purpose

- Implement `kernel.restart` as declared by the adjacent authoritative TOML.

# Ownership

- Own only delegation to `lifecycle.Manager.RequestRestart` and acknowledgement.
- Do not stop services or replace the process directly.

# Local Contracts

- Public API: handler constructor `New(*services.Services) core.Handler`.
- There is no short alias; the first lifecycle request wins.
- The command remains callable while the command server drains so repeated
  restart requests are idempotent.

# Work Guidance

- Keep graceful cleanup and process replacement in `kernel/app`.

# Verification

- Application integration validates restart selection and graceful cleanup;
  process verification confirms replacement loads the current executable.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
