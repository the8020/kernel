Parent DOX: [logging DOX](../AGENTS.md).

# Purpose

- Own the dedicated log writer's ingestion, segments, rotation, retention, and
  bounded readers, independently of kernel settings and runtime dependencies.

# Ownership

- A single file-owner loop serializes file mutations and prepared policy
  changes. Producers use a separate bounded queue; filesystem delays never hold
  kernel control locks. The kernel owns process lifecycle and the authoritative
  policy.

# Local Contracts

- `Run` receives initialization and privileged controls on an inherited private
  Unix connection. Registered node/sandbox credentials authenticate one
  persistent producer connection; no ordinary record invokes the kernel. A
  bounded listener admission gate allows at most 32 simultaneous unauthenticated
  handshakes.
- Bind node and source once; supervisors also have one fixed sandbox. Reject
  attempted relabeling and retain kernel-owned sandbox/Worker event attribution.
  Report four upstream loss counters, queue losses, malformed packets, storage
  failures, policy revision, and current segment accounting through status.

- Own only `segment-<20-digit-number>-<all|kernel|deno|logd>.log` regular files,
  the durable segment counter, and the writer lock in the node log directory.
  Segment numbers are reserved durably before creation and never reused after
  retention or restart. No per-record sequence is stored.
- Scan owned segments once at startup, then maintain byte totals and an expiry
  heap. Ordinary writes never rescan the directory. Total retention includes
  active bytes; close/rotate an active segment before it can be deleted.
- Open replacement files before closing usable current outputs. Failed writes
  roll back the incomplete batch when possible; failed rollback seals the
  damaged segment so future records start in a new file. Readers exclude
  incomplete tails.
- Age expiry is based on a segment's last append time, including idle active
  files. Time/size rotation bounds how much older material shares that segment;
  a segment is retained until its newest appended data reaches the age limit.
  Empty active files do not repeatedly rotate while idle.
- Policy preparation opens replacements without publishing them. Discard removes
  prepared empty files; commit changes all streams together. Disabling writes
  keeps existing segments readable. Retention remains active while disabled.
- During policy preparation the file owner holds queued intake within its normal
  budget until commit/discard; it does not rotate past reserved replacement
  segment IDs. Preparation must have a bounded lifetime in the control protocol.
- Preparation expires after five seconds. Commit publishes one complete policy
  and coalesces producer updates; queued records are filtered under the new
  policy before writing. Storage failures drain/drop and retry at most once per
  second. Loss/recovery summaries are limited to one per ten seconds.
- Kernel control EOF or shutdown starts a two-second input drain, followed by
  bounded forced reader closure and final dirty sync. No parent-death kill
  signal is used. A log-directory flock excludes duplicate writers before the
  socket is replaced; kernel lifecycle owns restarting the process.
- Native sandbox transport uses owned FIFOs in `kernel-api/log-ingress`.
  Registration reuses the existing inode across logger restart. The kernel owns
  FIFO creation/removal and standby drainage while logd is unavailable.
  Unregistration follows native process stop and acknowledges a short available
  byte drain before the kernel removes its unchanged FIFO inode. Raw stdout is
  INFO and stderr ERROR; stream origin is preserved without claiming to recover
  application severity or anonymous Worker context.
- Private kernel control may recover a sandbox with an empty token for raw FIFO
  reading only. Reject every socket handshake for that binding until
  registration supplies its original 64-hex credential. Promotion preserves the
  raw readers and inode; an already authenticated binding cannot change
  credentials.
- WriterActive probes the existing writer lock without creating files so the
  kernel's standby readers avoid competing with a previous draining writer.
- Query cursors encode a version, filter fingerprint, and at most four
  stream-specific segment/byte positions. Filtering advances positions without
  retaining records. Normal rotation and writer restart preserve cursors;
  removed segments report `expired`, missing files `unavailable`. There is no
  per-reader or per-context registry. Follow uses bounded cursor polling.
- Status publishes a filter-independent starting position for the current end of
  each stream. The kernel caches this constant-size reference; taking one for an
  execution adds no file read or control RPC. Query.Position applies a fresh
  filter at those offsets, then returns ordinary filter-bound continuation
  cursors. It cannot be combined with Tail or Cursor. Old references retain the
  same expired/unavailable semantics as other segment references.
- Unified queries preserve file order; split queries merge the next available
  record from each stream by capture time. This is not a distributed causal
  order. Tail inspects at most the last 256 KiB per stream, returns the bounded
  newest matching view, and reports `tail_limited` when older content lies
  outside that window. Interrupted or malformed lines are skipped and counted.

# Work Guidance

- Keep dependencies narrow: standard library, Linux OS primitives, shared
  operational IDs, and the logging record/wire contract. Do not import
  application, settings, database, sandbox, or command-bus packages.

# Verification

- Go tests exercise size/time splits, global totals across active streams, idle
  age expiry, counter continuity, policy preparation/rollback, partial writes,
  failed rotation, storage recovery, and preservation of unrelated files.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
