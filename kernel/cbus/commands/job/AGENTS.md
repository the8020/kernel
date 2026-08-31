# Purpose

- Expose Phase 1B job submission, durable execution records, inspection, and cancellation.

# Ownership

- Own declarative job run/list/inspect/cancel handlers.

# Local Contracts

- `job list` exposes only execution/job identity, state, owner, detached status, duration, and any failure; `job inspect` owns inputs, results, logs, permissions, runtime identity, and complete timing.
- Input is one validated JSON value; detached jobs return their durable execution identity immediately even when bounded admission leaves them queued.
- `job run --workspace` requests one instance-root-bounded `/workspace` mount;
  `--workspace-write` is required for host writes.

# Work Guidance

- Queue bounds, reuse, timeouts, and parallelism remain job-manager policy.

# Verification

- Generated validation and handler tests cover every job command and option.

# Child DOX Index

- This domain contract owns its leaf command folders; they contain only one declarative command and thin handler each.
