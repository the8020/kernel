Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own structured kernel file logging and runtime rotation/retention policy.

# Ownership

- Own the `slog` logger, concurrent writer, UTC period splitting,
  record-preserving size rotation, owned-file cleanup, enable/disable, policy
  preparation, and graceful closure.
- Do not log command-specific presentation or delete non-kernel files.

# Local Contracts

- Public API: `ErrInitialization`, `Policy`, `Manager`, `New`,
  `PolicyFromValues`, `Logger`, `ActiveFile`, `Enabled`, `Write`, `Prepare`, and
  `Close`.
- Files are `kernel-*.log` under `node/kernel/logs`; cleanup never deletes the
  active file and removes oldest closed owned files first.
- Retention cleanup runs at manager startup and after rotation, never for every
  ordinary record; the write hot path performs only boundary checks and append.
- A replacement writer is opened before persistence, atomically swapped on
  commit, and removed on discard.
- UTC boundaries support none, minute, hour, day, week, month, and year.
- Extend only for a current kernel logging acceptance criterion.

# Work Guidance

- Use `log/slog` and this small writer; do not add a logging framework.
- Keep logging policy generic: record transport, formatting, batching, writing,
  and retention. Request measurement and decisions to emit failure,
  slow-request, or statistical summaries belong to the emitting application or
  owning runtime module, not HTTP-specific logging settings.
- Compact human-readable prefixes show recognizable typed IDs without redundant
  labels such as `sandbox=` or `worker=`; declared-object names remain distinct
  from runtime-instance IDs.
- When limiting a record, preserve its identifying metadata and the beginning
  and end of oversized message or stack text. Omit the middle with an explicit
  marker, preserve valid encoding, and bound memory during formatting.
- Unified logging must use small byte-bounded transport and batch buffers;
  supervisor memory must not retain per-Worker log histories.

# Verification

- `logging_test.go` covers all period boundaries, default-enabled file creation,
  size rotation, total cleanup, active preservation, disable/re-enable, policy
  replacement failure, and prepared-file discard.
- Tests also preserve startup cleanup and prove ordinary non-rotating writes do
  not trigger retention scans.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
