# Purpose

- Expose the kernel-owned system database through typed administrative commands.

# Local Contracts

- `database check` verifies connectivity without mutating configuration.
- `sql` runs one bounded query by default or one statement with `--execute`.
- Handlers delegate to `services.DatabaseService`; they never open connections
  or interpret application schemas.

# Verification

- Handler tests cover connectivity, query/execute routing, parameters, and
  structured failures.
