Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Define the small schema-deployment handshake shared by package synchronization
  and development activation.
- This package contains contracts only; Git, database, evaluator, and service
  lifecycle behavior stay with their owning packages.

# Local Contracts

- `Prepare` finishes before activated package files become visible.
- `Candidate.Commit == ""` explicitly removes the selected package; `Root`
  remains its installed path. Nonempty commits publish candidate source. Both
  forms use the same preparation, completion, and recovery handshake.
- `Complete(true)` records the code switch; `Complete(false)` recovers catalog
  metadata to the still-active package set.
- The shared handshake binds both calls to the same explicit `act-` ID.
  Package-scoped source locks cover preparation, source switching and
  completion; independent package sets can progress separately. A late
  completion may never consume another activation's preparation. Package and
  database tests cover the shared handshake.
- The normal kernel installs a rejecting placeholder before command handlers are
  exposed, then replaces it with the evaluator once runtime composition reaches
  that point. Package mutations therefore fail closed during early runtime
  failure; the separate offline first-install synchronization intentionally has
  no database yet.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
