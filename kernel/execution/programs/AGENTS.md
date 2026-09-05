Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Resolve ready package programs and submit them through the ordinary job
  system.

# Local Contracts

- A program is a package artifact; a job is one invocation of that artifact.
- `Run` submits package CBus commands as `system`, retaining the caller context
  for synchronous child-job admission. `RunWithOptions` forwards the runtime
  caller's execution user, optional sandbox placement group, and timeout. Both
  use the same job path and own no durable scheduling or history.
- Use the ordinary job runtime profile, complete shared read-only packages
  mount, permissions, grouping, capacity, and configured Worker reuse policy.
  Commands may import and call other packages just like services and jobs.
- Never copy or snapshot package sources, create invocation directories, add
  command-specific mounts or sandboxes, or hold repository locks while running.
  Package activation owns source publication; the runner does not preserve a
  private release tree for self-updates or invent another isolation boundary.
- Resolve the ready active record, manifest, containment, and real non-symlink
  entrypoint. Reject a stale catalog commit before job submission. The commit
  identifies the active release at resolution, not an immutable per-run mount.
- Forward arguments and separate secure inputs to jobs. The shared job and
  supervisor layers own defensive input copies, argument spreading, secret
  cleanup/redaction, and Worker lifecycle. Do not duplicate those mechanisms.
- Return job errors unchanged, including structured supervisor failures and Go
  error causes. The Worker uses job mechanics with `program` origin and the
  logical program ID.

# Verification

- Tests cover system command identity, preserved caller context, default job
  policy, runtime execution options, stale/resolution failures, and job error
  identity.
- The rootless backend E2E test dispatches a CBus command through the real job
  and Worker managers into Deno, verifies static/dynamic cross-package imports,
  system identity, normal mounts, no package artifacts, and Worker cleanup.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
