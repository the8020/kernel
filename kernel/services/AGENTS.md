Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Supply generated handlers with the exact typed Phase 1 dependencies.

# Ownership

- Own `Services`, `RuntimeServices`, immutable `InstanceInfo`, and narrow
  handler-facing cryptographic, database, secrets, package, development, and
  web-service interfaces only.
- Do not perform lookup, lifecycle behavior, validation, or domain operations.

# Local Contracts

- Transient package synchronization credentials include an optional HTTP Basic
  username and a secure token/password; the package owner confines their use.

- Public API: `InstanceInfo`, `Services`, `RuntimeServices`, `RuntimeSnapshot`,
  `PublishRuntime`, narrow handler-facing domain interfaces, and `New`.
- Fields are limited to settings, network, shared node topology/capacity,
  logging, lifecycle, deployment signing, instance status, system-database
  status/raw SQL/catalog/synchronization operations, named-secret list/get/set,
  package discovery/index/synchronization/repository operations, development
  image/workspace/activation operations, selected isolation diagnostics,
  low-level runtime pools, and exact operations used by current handlers/runtime
  bridges.
- The platform snapshot exposes the shared console manager to typed terminal
  operations; it introduces no second PTY broker.
- The web-service interface exposes generic soft/hard restart with an optional
  monotonic update revision for cross-node deduplication; the owning lifecycle
  implementation supplies the behavior and persistence.
- Runtime dependencies expose the generic local event dispatcher, ordinary
  program runner, and program-catalog reader to the private operations bridge;
  the native event command uses the same dispatcher. `Reindex(ctx, packageIDs)`
  exposes shared command/event/hook indexing to the native command; an empty
  selection means all packages.
- Infrastructure `Failure` prevents runtime commands; `ApplicationFailure`
  reports application initialization separately and does not revoke native jobs,
  eval/run, or SQL repair. Unavailable application dependencies remain nil.
- Runtime initialization publishes a complete immutable infrastructure snapshot
  before schema work, then replaces it with the completed application snapshot.
  Published structs are never mutated; both publications use synchronization.
- Extend only when a generated handler has a current typed dependency.
- Package management includes `DeletePackage` for the confirmed CBus/private
  operation; package activation owns removal and recovery.
- Signing is available before database/runtime startup and exposes no private
  key material. User/session policy is not a kernel dependency.
- The sandbox handler contract exposes live lifecycle operations separately from
  bounded history listing and direct history inspection. Cached inspection and
  targeted live `Refresh` are separate operations so list/navigation paths never
  imply a supervisor scan.

# Work Guidance

- Never turn this into an untyped service locator.

# Verification

- Generated-registry compilation, all-handler success/degraded tests, and
  application integration verify the dependency contract.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
