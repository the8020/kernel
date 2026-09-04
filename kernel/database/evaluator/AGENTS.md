# Purpose

- Discover activated package table modules and evaluate them through the normal
  sandboxed job runtime.
- Keep TypeScript execution out of the Go process and database credentials out
  of evaluator Workers.

# Local Contracts

- Discovery is fixed-depth at `tables/<table>.ts` and uses the centralized
  canonical table ID encoder.
- Evaluate at most 256 modules per job call, type-check every requested module,
  commit each successful initialization batch, and reuse one compatible
  evaluator Worker when possible. Failed first initialization resumes completed
  table/commit pairs.
- Mount and read only the activated shared package tree. Private development
  overlays are never schema sources; staged activation roots temporarily replace
  only their matching package mounts.
- Explicit inspection and synchronization accept activated source only when its
  checkout is clean and exactly matches the ready commit in the shared package
  index. Candidate activation evaluates its isolated staged root instead.
- Depend only on the read-only package catalog. Database-backed desired package
  and service state is composed after initial table synchronization.
- Evaluator Workers have no database access, no writes, imports,
  external network, administration, or direct credentials.
- Initial and explicit full synchronization discovers all package tables.
  Ordinary deployment reevaluates only new/changed/deleted definitions and
  tables whose recorded Deno module dependency closure intersects Git's changed
  files. There is no custom TypeScript or dynamic-import parser.
- `Prepare` applies candidate schema before source visibility and `Complete`
  either records activation or restores active descriptors. Restart recovery
  evaluates the package tree actually present on disk.
- PostgreSQL retains one database advisory lock from `Prepare` through source
  switch and `Complete`; recovery aligns schema and clears the pending record
  under that same lock. SQLite uses the manager's local schema lock.

# Verification

- Tests cover identity, collision rejection, batching, package fingerprints,
  and malformed evaluator results.

# Child DOX Index
