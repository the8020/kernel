# Purpose

- Own the kernel system-database pool, built-in 80|20 catalog, physical schema
  synchronization, runtime SQL execution, and transaction scopes.

# Ownership

- Open SQLite or PostgreSQL, apply pool/result policy, expose credential-free
  status, bootstrap `_8020_*`, introspect physical schemas, and store normalized
  package table descriptors.
- The Go kernel is the only holder of database credentials. Sandboxed code uses
  the authenticated runtime callback API.
- Kernel-owned domain repositories receive only the small internal `Store`
  interface. The connection pool itself remains private to this package.
- `RelationExists` supplies engine-specific catalog lookup to distinguish a
  missing table/view from a failed read without string-matching driver errors.
  PostgreSQL lookup does not hide relations merely because SELECT is denied.

# Local Contracts

- SQLite is the single-node default. It stores `system.db` beneath the mapped
  instance database root, uses strict tables and WAL, and serializes schema work
  locally. PostgreSQL uses one database advisory lock across initialization and
  deployment.
- Readiness distinguishes `UNAVAILABLE`, `CONNECTED`, `INITIALIZING`, `READY`,
  and `INITIALIZATION_FAILED`. Catalog failure blocks the service plane but
  never the command socket or raw SQL recovery path.
- Embedded engine SQL owns only `_8020_catalog`, `_8020_tables`,
  `_8020_columns`, `_8020_dependencies`, and `_8020_pending_deployment`. Every
  non-catalog table comes from an activated package TypeScript descriptor.
- A fresh database synchronizes all installed definitions in bounded batches
  before becoming initialized. An initialized ordinary boot validates only the
  small catalog contract and pending deployment state; full definition and drift
  checks are explicit operations. An existing catalog is validated without DDL
  or insert attempts, so startup can read alongside an active SQLite writer.
  Invalid existing catalogs fail validation instead of being silently repaired.
- Canonical table IDs are also physical names. Synchronization creates missing
  structures and safe additions, retires removals without deleting data, and
  returns `migration_required` for unsupported changes. Only confirmed `Trim`
  deletes selected retired objects. Raw administrative DDL remains available.
- Catalog list and table inspection never evaluate activated TypeScript.
  Per-table comparison and definition scans are explicit deeper operations so
  routine administration remains fast.
- Package/schema switching uses one durable pending record. Candidate schema is
  prepared before source replacement; completion records the active commit set.
  Restart recovery aligns catalog state to the package tree that is actually
  active. The last failed deployment remains visible without degrading an
  otherwise ready database.
- Runtime SQL has one unified row/non-row operation and opaque kernel-held
  transactions bound to an exact Worker-execution plus request/invocation scope.
  Acquisition obeys the caller deadline; the transaction then has its own
  bounded lifetime. Request cleanup rolls back its exact scope and Worker exit
  rolls back the scope prefix, with every connection/permit release idempotent.
  Optional transaction `timeoutMs` bounds acquisition and total lifetime,
  including cancellation of an active statement;
  `lockTimeoutMs` uses PostgreSQL transaction-local lock_timeout or SQLite
  connection-local busy_timeout with restoration before pool reuse. Values
  default to existing behavior; short application claims can fail promptly.
  Mutations return an insert ID only when the caller
  explicitly identifies an insert, preventing connection-local stale IDs from
  leaking into updates or deletes. Values use explicit lossless tags for bigint,
  decimal, datetime, bytes, and JSON.
- Kernel-owned repositories normalize engine-native stored values through this
  package's shared encoders and decoders. Sandboxed package CRUD uses the
  descriptor-aware `/p/the8020/db/mod.ts` codec; deliberately raw SQL results remain
  engine-native where their logical type cannot be inferred without parsing SQL.
- Backend SQL syntax, placeholder handling, execution transport, and physical
  value decoding are shared database concerns. Fix dialect discrepancies here or
  in `/p/the8020/db/mod.ts`'s compiler/driver as their contract dictates; never make an
  individual service, login flow, or repository compensate for them.
- Query results fail, rather than truncate, above the runtime-mutable per-node
  row/byte limits. Defaults are 10,000 rows and 10 MiB. Pool defaults are 32
  open and 8 idle connections; pools grow on demand. Application statements and
  transactions share a cancellable admission gate capped at open-minus-two when
  possible, while kernel-owned repositories bypass it so readiness,
  authentication, and administration retain pool access.
- SQLite file permissions are established after the first successful open, not
  re-applied by every readiness ping. Readiness uses lightweight connection and
  catalog checks rather than repeated full definition scans.
- Decimals are canonical strings in TypeScript and signed scaled 64-bit integers
  in both engines. Integers are physically signed 64-bit but limited to the
  JavaScript safe range. Datetimes are UTC milliseconds. Physical foreign keys,
  streaming, savepoints, destructive automatic migration, and a generalized
  migration framework are intentionally deferred.

# Verification

- Tests cover catalog readiness/idempotence/failure, existing-catalog startup
  alongside a held writer, SQLite WAL and schema
  synchronization, naming and descriptors, safe/unsafe changes, drift,
  retirement/trim, pending recovery, deployment outcome visibility, logical
  references, exact values, deadline-bound transaction acquisition and cleanup,
  application/kernel pool isolation, concurrent runtime read load, pool
  pressure, and configurable result bounds.
- PostgreSQL JSON parameters and short conditional claims have an optional live
  regression test. Set `THE8020_TEST_POSTGRES_LOCATION` and, if needed,
  `THE8020_TEST_POSTGRES_USERNAME` to a disposable PostgreSQL database; the test
  creates and removes its own uniquely named table. SQLite tests also verify
  short lock waits and restoration of pooled connection settings.

# Child DOX Index

- `evaluator/AGENTS.md`: sandboxed activated-package definition evaluation.
