Parent DOX: [kernel/kernel/sandbox DOX](../AGENTS.md).

# Purpose

- Preserve terminal sandbox metadata and unified-log references outside live
  sandbox state.

# Ownership

- Own immutable history records, direct retained-ID markers, hour-partitioned
  append-only indexes, bounded explicit history queries, and metadata retention.
- Do not own live sandbox lifecycle, backend cleanup, command presentation, or
  UUI rendering.

# Local Contracts

- History lives under its own private runtime root and is never scanned by live
  sandbox operations.
- Retained sandbox-ID markers are indexed once at startup and maintained with
  archive/cleanup, so collision checks do not stat files during admission.
- Recent listing reads bounded index tails; direct inspection derives one
  sharded record path from the history ID.
- Cleanup removes expired hour buckets and their retained-ID markers without
  walking live state or individual archive directories.
- Schema 2 records retain the ordinary specification and terminal status,
  including the creation node, creation time and saved log position. Inspection
  reads only metadata; logd owns the separately queried log files and their
  retention. History never copies, moves, reads or deletes those files.
- Archived specifications use the ordinary secret-omitting JSON contract;
  internal callback tokens never enter metadata, indexes, or log responses.

# Work Guidance

- Publish metadata before its index and ID marker become visible.

# Verification

- Unit tests cover archive/list/inspect with preserved log references, direct
  retained-ID checks, bounded pagination, and independent metadata cleanup.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
