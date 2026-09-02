# Purpose

- Own the kernel's system database connection pool and bounded SQL boundary.

# Ownership

- Resolve database configuration, open SQLite or PostgreSQL through
  `database/sql`, apply pool policy, check connectivity, report pool pressure,
  normalize query values, and bound results.
- The Go kernel is the only database client. Sandboxed code reaches this owner
  only through the authenticated runtime callback API.

# Local Contracts

- SQLite is the zero-configuration single-node default and stores
  `system.db` in the instance-owned `database/` directory. It uses WAL to allow
  readers alongside its one writer and therefore requires local filesystem
  semantics.
- PostgreSQL uses a credential-free connection URL plus separately configured
  username and password. Configuration is global and restart-required.
- Both backends use the node-local runtime-mutable maximum-open and
  maximum-idle pool policy, defaulting to 32 and 8. Pools open connections on
  demand; the idle limit retains warm connections but does not cap active use.
- Connection failure never prevents the kernel control plane from starting.
- Status performs no database I/O: it combines cached readiness with local
  `database/sql` open, in-use, idle, wait-count, and wait-duration counters.
- `Query` and `Execute` accept one statement and scalar parameters. Query
  results are ordered column/row arrays and are bounded by row and byte limits.
- `$1`, `$2`, and so on are the portable positional-parameter form supported by
  both configured backends; the kernel does not rewrite SQL.

# Non-Responsibilities

- No schema ownership, migrations, ORM behavior, application table naming,
  sandbox placement, or runtime lifecycle policy.

# Verification

- Tests cover configuration, SQLite WAL read/write concurrency, creation and
  permissions, live pool resizing and pressure reporting, parameterized
  query/execute behavior, value normalization, and result bounds. Benchmarks
  compare read-only and mixed traffic at 8, 16, 32, and 64 connections.
