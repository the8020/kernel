Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own one generic sandbox and Worker execution system shared by services
  and jobs.

# Ownership

- Own sandbox placement/compatibility, warm capacity, supervisor communication,
  Worker registry, exact Worker invocation, service pools/dispatch, in-memory
  job admission, execution artifacts, and runtime reconciliation coordination.
- Do not own containerd/gVisor/CNI/cgroups, host-port listeners, command
  presentation, or program business logic.

# Local Contracts

- All workload types use the same sandbox manager, supervisor protocol, Worker
  bootstrap, runtime profile, permissions, mounts, resources, and debugging
  path.
- A sandbox has exactly one workload type; shared owners share its
  process/security/resource/failure boundary.
- Each service sandbox owns an independent internal stateless or persistent
  Worker pool with hard per-Worker execution-slot limits. The kernel owns the
  service-wide desired Worker count and sandbox placement; jobs normally own one
  Worker per execution behind a bounded in-memory FIFO admission queue and leave
  no execution history after completion.
- Every Worker starts with a validated execution user and an outer origin of
  service, direct module, or package program. Runtime calls carry that user with
  their caller execution through Go context; child jobs inherit it. Jobs without
  a caller require explicit assignment. Kernel-owned operations assign `system`
  explicitly, including package CBus commands regardless of their caller.
  Canonical principal validation is independent of account tables for every
  username, including system. No manager queries account state or invents a
  fallback identity. Synchronous child-job admission discounts its waiting
  parent, preventing a bounded Worker pool from deadlocking on its own
  dependency.
- `Invocation` is the shared Go context contract: `ContextID` identifies one
  invocation, `ParentContextID` correlates its caller, and optional `JobRunID`
  identifies the owning job run. Worker metadata contains no first-invocation
  ID. Job startup/import and execution share the submitted context; each reuse
  receives a new context. Exact Worker calls create a child context, including
  across authenticated node forwarding.
- `Caller.Valid` requires a canonical `ctx-`, optional `job-`, workload, and
  principal. Runtime ingress rejects malformed callers before attaching them;
  invalid parent identities must never become an uncorrelated child invocation.
- Runtime capability ingress also stamps the canonical sandbox/Worker pair in
  `Caller`. Native replaceable resource leases use this pair, independent of
  request or authentication-session lifetime. The optional pair is validated
  together; kernel-originated callers need not invent Worker identities.
- Newly generated sandbox IDs are `sbx-` plus ten random lowercase
  alphanumeric characters; newly generated Worker IDs are the equivalent `wrk-`
  format.

# Work Guidance

- Change execution foundations only for a necessary capability shared by
  ordinary workloads. Keep application scheduling, authentication, session
  protocols, and history in Deno packages.
- Make transport, logical execution, Worker, and sandbox ownership explicit in
  every lifecycle change. Preserve exact identity, bounded admission,
  cancellation, and idempotent cleanup, and verify the affected service/job
  path.

- Express workload differences only through grouping, lifecycle, scaling,
  permissions, mounts, routing, and scheduling; never create separate runtime
  backends.

# Verification

- Unit tests cover grouping, warm accounting, supervisor protocol, exact Worker
  invocation, and Worker/service/job state and scheduling; gVisor integration
  tests cover real Deno execution and cross-workload invariants.

# Child DOX Index

- [adminrun/AGENTS.md](adminrun/AGENTS.md): bounded eval/run artifacts submitted
  through ordinary jobs.
- [coordinator/AGENTS.md](coordinator/AGENTS.md): generic grouping selection and
  sandbox construction.
- [placement/AGENTS.md](placement/AGENTS.md): placement/sharing keys and
  compatible sandbox selection.
- [jobs/AGENTS.md](jobs/AGENTS.md): bounded FIFO admission, synchronous/detached
  jobs, cancellation, and compatible Worker reuse.
- [pool/AGENTS.md](pool/AGENTS.md): real clean-sandbox provisioning, assignment,
  trimming, and asynchronous replenishment.
- [profile/AGENTS.md](profile/AGENTS.md): safe Worker-permission-derived runtime
  profiles and online dependency separation.
- [programs/AGENTS.md](programs/AGENTS.md): ready package entrypoint resolution
  and ordinary job submission, with system command identity or explicit program
  execution options.
- [records/AGENTS.md](records/AGENTS.md): restrictive atomic workload registry
  documents.
- [services/AGENTS.md](services/AGENTS.md): durable independent service pools,
  scaling, and streaming routes.
- [supervisor/AGENTS.md](supervisor/AGENTS.md): authenticated, streaming
  kernel-to-Deno supervisor client.
- [workers/AGENTS.md](workers/AGENTS.md): generic Worker validation, lookup,
  lifecycle, and workload delegation.
