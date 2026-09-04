# Purpose

- Supply generated handlers with the exact typed Phase 1 dependencies.

# Ownership

- Own `Services`, `RuntimeServices`, immutable `InstanceInfo`, and narrow
  handler-facing authentication, database, secrets, package, development, and
  web-service interfaces only.
- Do not perform lookup, lifecycle behavior, validation, or domain operations.

# Local Contracts

- Public API: `InstanceInfo`, `Services`, `RuntimeServices`, `RuntimeSnapshot`,
  `PublishRuntime`, narrow handler-facing domain interfaces, and `New`.
- Fields are limited to settings, network, shared node topology/capacity,
  logging, lifecycle, request authentication, instance status,
  system-database status/raw SQL/catalog/synchronization operations, named-secret
  list/get/set, package discovery/index/synchronization/repository operations,
  development image/workspace/activation operations, selected isolation
  diagnostics, low-level runtime pools, and exact operations used by current
  handlers/runtime bridges.
- Both runtime diagnostics remain present when initialization fails; unavailable
  lifecycle fields stay nil with one safe failure string.
- Runtime initialization publishes only complete immutable dependency snapshots
  under synchronization; command handlers never read a partially composed
  runtime.
- Extend only when a generated handler has a current typed dependency.
- Authentication exposes only request identity and cookie operations;
  package-owned user/session administration does not enter this dependency set.
- The sandbox handler contract exposes live lifecycle operations separately
  from bounded history listing and direct history inspection.

# Work Guidance

- Never turn this into an untyped service locator.

# Verification

- Generated-registry compilation, all-handler success/degraded tests, and
  application integration verify the dependency contract.

# Child DOX Index
