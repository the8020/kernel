Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Own non-durable job execution on job Workers in generic runtime groups.

# Ownership

- Enforce configured parallelism, bounded in-memory FIFO queueing, timeouts,
  synchronous/detached execution, argument and secure-input delivery,
  cancellation, and optional compatible idle-Worker reuse.
- Do not persist, replay, schedule, or create automatic history for ordinary
  jobs. Durable calendars and history belong to the `the8020/jobs` Deno package;
  generic event dispatch submits listeners through this same ordinary job path.

# Local Contracts

- Public API: `New`, `Manager.Run`, `List`, `Inspect`, `Cancel`, `FailGroup`,
  `Close`, and policy/options/record types.
- `Options.Arguments` is the positional array spread into the job's default
  export. `Options.Secrets` travels separately and is cleared after every path.
- Only queued, starting, running, and reusable-idle state is retained in memory.
  Terminal result/output is returned to the caller and immediately removed.
- Secure values are scrubbed from returned values, captured logs, and failures.
  Redaction preserves structured execution error classification and details;
  secret-free failures retain their original Go cause.
- One started execution maps to one Worker unless explicit compatible reuse is
  enabled. Sandbox selection requests capacity for that one Worker. Each Worker
  owns one sandbox allocation claim. Non-reusable Workers stop and release that
  claim after one call; reusable Workers retain it until idle retirement.
  Package commands follow this same policy without caller-specific overrides.
- An explicit instance-root-bounded development workspace becomes an
  owner-scoped runtime-profile mount at `/workspace`; writable access is opt-in.
  Additional staged activation/evaluator mounts must be read-only and enter
  profile compatibility; ordinary package commands use the existing mounts.
- Jobs use the same runtime image, package/runtime read access, kernel API,
  writable temp/cache paths, and network/import access as services.
- A job requires an explicit canonical principal or inherits its synchronous
  runtime caller. Account rows, enabled state, passwords, and sessions are
  outside admission. System and other principals follow the same structural
  validation. Queued and reused jobs retain their principal independently of
  account changes. The immutable origin is the job ID or the resolved package
  program ID.
- Compatible Worker reuse includes user and origin identity; work from different
  users or origins never shares a reusable Worker.
- `Options.CheckModules` asks the supervisor to type-check a bounded module list
  before import. Database access may be full, metadata-only, or absent.
- An execution timeout covers queueing, sandbox acquisition, Worker startup,
  validation, and invocation. Cancelling queued work starts no Worker.
- Optional `PlacementGroup` uses the same exact sandbox-group value contract as
  services, scoped to job workloads. Preparation copies the selected value so
  queued work cannot change placement through caller mutation.

# Work Guidance

- Count `STARTING`/`RUNNING` against parallelism, count only `QUEUED` against
  queue capacity, and never reuse across incompatible identities or profiles.
  The global limit applies across logical jobs; an invocation parallelism limit
  applies only to matching job IDs. A synchronous child discounts its validated
  waiting job parent from global admission, but no other active work.
- Queue admission is event-driven. State changes broadcast one shared wake-up;
  do not add per-job tickers or polling scans.

# Verification

- Unit and race tests cover results/output, secure redaction and cleanup,
  detached FIFO behavior, queue bounds, cancellation, timeout, mounts, database
  metadata, module checking, default destruction, compatible reuse, idle
  retirement, no persistence/replay, and group failure.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
