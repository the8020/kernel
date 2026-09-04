# Purpose

- Adapt kernel-owned database primitives for package runtime operations.

# Local Contracts

- No public CBus metadata lives here; `the8020/db` owns visible `db.*` and
  `db.tables.*` command programs.
- The private `database.check` operation verifies connectivity without mutating
  configuration.
- `sql` runs one bounded query by default or one statement with `--execute`.
- `table list` is database-centric, `table definitions` evaluates activated
  source explicitly, `table inspect` performs detailed catalog/physical/source
  comparison, and sync/trim commands share the kernel database owner.
- Handlers delegate to `services.DatabaseService`; they never open connections
  or interpret application schemas.

# Verification

- Handler tests cover readiness, raw query/execute routing, parameters, table
  administration routing, and structured failures.
