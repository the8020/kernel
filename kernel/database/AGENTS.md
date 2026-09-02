# Purpose

- Own the kernel's single system database connection and bounded SQL boundary.

# Ownership

- Resolve database configuration, open SQLite or PostgreSQL through
  `database/sql`, check connectivity, normalize query values, and bound results.
- The Go kernel is the only database client. Sandboxed code reaches this owner
  only through the authenticated runtime callback API.

# Local Contracts

- SQLite is the zero-configuration single-node default and stores
  `system.db` in the instance-owned `database/` directory.
- PostgreSQL uses a credential-free connection URL plus separately configured
  username and password. Configuration is global and restart-required.
- Connection failure never prevents the kernel control plane from starting.
- `Query` and `Execute` accept one statement and scalar parameters. Query
  results are ordered column/row arrays and are bounded by row and byte limits.
- `$1`, `$2`, and so on are the portable positional-parameter form supported by
  both configured backends; the kernel does not rewrite SQL.

# Non-Responsibilities

- No schema ownership, migrations, ORM behavior, application table naming,
  sandbox placement, or runtime lifecycle policy.

# Verification

- Tests cover configuration, SQLite creation and permissions, parameterized
  query/execute behavior, value normalization, and result bounds.
