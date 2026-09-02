# Purpose

- Expose database-first table catalog inspection and explicit synchronization.

# Local contracts

- Handlers delegate to `services.DatabaseService`.
- Normal synchronization is additive and retires removals; only `trim` destroys
  retired physical objects.
- Definition listing is a separate activated-source comparison, not a merged
  catalog view.
