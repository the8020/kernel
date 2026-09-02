# Purpose

- Define the small schema-deployment handshake shared by package synchronization
  and development activation.
- This package contains contracts only; Git, database, evaluator, and service
  lifecycle behavior stay with their owning packages.

# Local Contracts

- `Prepare` finishes before activated package files become visible.
- `Complete(true)` records the code switch; `Complete(false)` recovers catalog
  metadata to the still-active package set.
- Callers serialize the handshake with their existing repository lock. The
  evaluator additionally retains the PostgreSQL advisory lock across both
  calls and the intervening source switch.
- The normal kernel installs a rejecting placeholder before command handlers are
  exposed, then replaces it with the evaluator once runtime composition reaches
  that point. Package mutations therefore fail closed during early runtime
  failure; the separate offline first-install synchronization intentionally has
  no database yet.

# Child DOX Index
