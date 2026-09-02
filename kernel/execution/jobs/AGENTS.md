# Purpose

- Own durable job-execution scheduling on job Workers in generic runtime groups.

# Ownership

- Enforce configured parallelism, bounded FIFO queueing, and timeouts; start synchronous or detached executions; send JSON input; persist results/logs/duration/failures; cancel one execution; and optionally retain/reuse a compatible idle Worker.
- Do not implement a separate job runtime, cron/workflow scheduling, distributed queues, or container-level cancellation for ordinary jobs.

# Local Contracts

- Public API: `New`, `Manager.Run`, `List`, `Inspect`, `Cancel`, `FailGroup`, `Restore`, `Close`, and policy/options/record types.
- One started execution maps to one Worker unless explicitly reusing a compatible idle Worker; active cancellation targets only that Worker.
- An explicit instance-root-bounded development workspace becomes an
  owner-scoped runtime-profile mount at `/workspace`; writable access is opt-in
  and participates in reuse/group compatibility.
- Jobs use the same managed Deno image and read-only installed-package mount as
  services. Each invocation supplies an execution context to the generic kernel
  bridge, including optional full, metadata-only, or absent database access.
- `Options.CheckModules` asks the existing supervisor validation path to
  type-check a bounded module list before the Worker imports its entrypoint.
  Extra mounts and permissions participate in runtime/Worker compatibility;
  this is the small reusable seam used by the table evaluator, not a separate
  job scheduler.
- Parallel saturation persists submissions as `QUEUED` up to `QueuedExecutionLimit`; admission follows durable submission order, detached runs return immediately, and synchronous runs wait under their caller context.
- An execution timeout bounds queueing, sandbox acquisition, Worker validation
  and startup, and program execution as one lifecycle.
- Cancelling queued work persists `CANCELLED` without touching a Worker. Detached work uses a bounded background context and persists every state transition. Non-reused Workers stop after completion or failure; reuse requires the same owner, job, entrypoint, release, runtime profile, and permissions, with one available record consumed atomically and retired after the configured idle timeout.
- Group failure fails active jobs while preserving completed results and retiring lost idle reuse capacity.
- Startup recovery never replays queued or ambiguous active work: it fails queued records, terminates active Workers, and marks those executions failed, while healthy idle reusable Workers regain their remaining retirement timer.
- An unreadable or identity-mismatched job record is quarantined without
  blocking recovery of other jobs or services.

# Work Guidance

- Count `STARTING`/`RUNNING` executions against parallelism, count only `QUEUED` records against queue capacity, and never reuse a Worker across incompatible job/entrypoint identities.

# Verification

- Unit and race tests cover synchronous results/logs/duration, detached FIFO
  completion, queue bounds, queued/caller cancellation, timeout failure,
  package mounts, database execution metadata, module checking, default
  destruction, compatible/release-incompatible reuse, idle retirement, startup
  recovery without replay, and group-failure propagation.
- Recovery tests also cover mixed valid and invalid record isolation.

# Child DOX Index
