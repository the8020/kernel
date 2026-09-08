Parent DOX: [kernel/kernel/sandbox DOX](../AGENTS.md).

# Purpose

- Persist small per-sandbox recovery records atomically under
  `node/kernel/runtime/sandboxes/` and provide their process-local routing cache.

# Ownership

- Own sandbox-directory layout, restrictive modes, atomic recovery writes, startup
  preload, indexed resolution, revisioned in-memory supervisor snapshots,
  synchronized state transitions, enumeration, and record deletion.

# Local Contracts

- Public API: `Store`, `New`, `SaveSpec`, `SaveStatus`, `UpdateStatus`,
  `Transition`, `TransitionIf`, `Load`, `Cached`, `Contains`, `List`,
  `Observe`, `Snapshot`, `ClaimStaleHeartbeats`, `RescheduleHeartbeat`,
  `ClaimIdle`, `RescheduleIdle`,
  `ObserveMetrics`, and `Delete`.
- `spec.json` stores desired immutable inputs, `state.json` stores observed
  status, and restrictive `secret.json` stores the internal callback/control
  token omitted from ordinary specification JSON. Source-of-truth distinctions
  remain intact.
- Writes use same-directory temporary files, sync, rename, and `0600`;
  directories use `0700`.
- `New` reads recovery files once. One sandbox index and cached tokens
  serve normal reads; absolute supervisor snapshots and diagnostic metrics
  remain memory-only and are reconstructed after restart.
- `Cached` never falls back to recovery files, including on a miss.
  Authenticated callback validation therefore cannot turn an unknown identity
  into disk I/O.
- Snapshot epoch plus revision rejects stale delivery. Equal revisions from the
  current epoch refresh heartbeat time without replacing state; older supervisor
  epochs cannot refresh a replacement's health.
- Ready/active/draining records live in an indexed heartbeat deadline queue.
  Monitoring claims a bounded number of stale IDs directly from that queue, so a
  periodic health pass never scans every cached sandbox.
- A second queue uses the same heap implementation for assigned sandboxes with
  no allocations or Workers. Accepted Worker-count transitions maintain
  `IdleSince`; equal heartbeat revisions never extend it. New ownership or live
  Workers remove idle eligibility. Failed cleanup remains eligible for retry;
  creation, draining, and reserved warm capacity stay outside this queue.

# Lifecycle

- Save validated spec before host creation, persist every observed transition,
  load during reconciliation, and delete only after runtime cleanup and terminal
  history archival succeed.

# Failure Behavior

- Corrupt or mismatched records fail reconciliation explicitly; failed writes
  preserve the previous complete file.

# Concurrency

- A small striped per-record lock serializes mutations of one sandbox;
  unrelated sandboxes proceed independently. One short RW lock protects process
  indexes and cached value copies and is never held across filesystem I/O.

# Dependencies

- Go JSON/filesystem standard library and `sandbox/model`.

# Non-Responsibilities

- No containerd observation, supervisor health, CNI, port state, logs,
  artifacts, or database behavior.

# Verification

- Unit tests cover atomic persistence, preload/index resolution, reload/list
  ordering, legal/illegal synchronized transitions, absolute snapshot ordering,
  memory-only observation, bounded heartbeat/idle indexing, Worker countdown
  reset, allocation protection, corruption,
  permissions, deletion, and concurrent independent records.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
