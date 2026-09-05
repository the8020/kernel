Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Provision, reserve, assign, trim, and asynchronously replenish clean warm
  runtime groups.

# Ownership

- Own profile templates, desired warm capacity, reconstruction of reconciled
  warm/assigned/failed accounting, actual warm sandbox creation, assignment
  through the sandbox manager, and background replenishment.
- Do not schedule Workers, choose logical group keys, create a second sandbox
  backend, or return used supervisors to clean capacity.

# Local Contracts

- Public API: `New`, `Controller.Start`, `Resize`, `Status`, `Assign`, `Forget`,
  and `Close`, plus `Template` and narrow sandbox dependency contracts.
- Warm sandboxes contain one healthy supervisor, no owners, no group key, and no
  Workers. Assignment is atomic at the pool boundary and immediately triggers
  creation of a new clean replacement.
- Profile hashes are derived from the complete immutable runtime profile; only
  registered profiles may be resized or assigned.
- Startup registers healthy clean warm groups before applying desired capacity,
  preserves assigned/failed accounting, and therefore never duplicates
  already-ready capacity after restart.
- Desired capacity zero is the default lazy mode: no clean sandbox is created
  until a workload request reaches the coordinator. Positive capacity remains an
  explicit prewarming choice.
- Warm provisioning requests compact collision-checked sandbox IDs from the
  sandbox manager and compact `rgp-` runtime-group IDs from the shared model
  generator.

# Work Guidance

- Keep provisioning idempotent and serialize reconciliation. Record failed warm
  capacity instead of presenting it as ready.

# Verification

- Unit tests prove initial provisioning, restart reconstruction without
  duplicate capacity, clean assignment, non-reuse, asynchronous replenishment,
  resize-down deletion, and unknown-profile rejection.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
