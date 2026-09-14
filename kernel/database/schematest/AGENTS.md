Parent DOX: [database DOX](../AGENTS.md).

# Purpose

- Exercise package-owned schema code through the native SQL and transaction
  implementation.

# Ownership

- Own the optional Deno test bridge used by database, activation, and
  composition tests.
- Production runs schema operations through ordinary sandboxed jobs; this helper
  is test-only.

# Local Contracts

- Run the normal sibling `db/internal/schema.ts` source, with an isolated
  loopback callback and execution-scoped native transactions.
- Require Deno on PATH and the sibling db source checkout; skip these
  cross-language tests explicitly when either prerequisite is absent. Never
  extract or patch implementation source.
- Keep tests sequential and close the HTTP listener, child process, and database
  scopes.

# Verification

- With Deno on PATH, `GOMAXPROCS=2 go test -p 1 ./kernel/database/...` exercises
  the bridge and schema regressions.

# Child DOX Index

No child DOX documents.
