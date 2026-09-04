# Purpose

- Expose live, non-durable job submission, inspection, and cancellation.

# Ownership

- Own declarative job run/list/inspect/cancel handlers.

# Local Contracts

- `job list` and `job inspect` expose only currently live in-process work;
  completed invocations are not retained.
- Input is one optional JSON-compatible argument. Detached jobs return their
  transient execution identity immediately even when bounded admission queues
  them.
- `job run --workspace` requests one instance-root-bounded `/workspace` mount;
  `--workspace-write` is required for host writes.

# Work Guidance

- Queue bounds, reuse, timeouts, and parallelism remain job-manager policy.

# Verification

- Generated validation, handler, and job-manager tests cover every command,
  option, and non-durable lifecycle.

# Child DOX Index

- This domain contract owns its leaf command folders; they contain only one declarative command and thin handler each.
