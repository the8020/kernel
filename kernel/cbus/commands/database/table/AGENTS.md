# Purpose

- Expose database-first table catalog inspection and explicit synchronization.

# Local contracts

- Handlers delegate to `services.DatabaseService`.
- Normal synchronization is additive and retires removals; only `trim` destroys
  retired physical objects.
- List and inspect are fast database-first operations and never evaluate
  TypeScript. Definition listing and per-table comparison are explicit,
  potentially expensive activated-source operations.
