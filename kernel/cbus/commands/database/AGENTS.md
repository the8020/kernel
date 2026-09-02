# Purpose

- Expose the kernel-owned system database through typed administrative commands.

# Local Contracts

- `database check` verifies connectivity without mutating configuration.
- `sql` runs one bounded query by default or one statement with `--execute`.
- `table list` is database-centric, `table definitions` evaluates activated
  source explicitly, `table inspect` performs detailed catalog/physical/source
  comparison, and sync/trim commands share the kernel database owner.
- Handlers delegate to `services.DatabaseService`; they never open connections
  or interpret application schemas.

# Verification

- Handler tests cover readiness, raw query/execute routing, parameters, table
  administration routing, and structured failures.
