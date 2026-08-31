# Purpose

- Preserve terminal sandbox metadata and bounded diagnostic logs outside live runtime-group state.

# Ownership

- Own immutable history records, direct retained-ID markers, hour-partitioned append-only indexes, bounded explicit history queries, log archival, and retention cleanup.
- Do not own live sandbox lifecycle, backend cleanup, command presentation, or UUI rendering.

# Local Contracts

- History lives under its own private runtime root and is never scanned by live sandbox operations.
- Recent listing reads bounded index tails; direct inspection derives one sharded record path from the history ID.
- Cleanup removes expired hour buckets and their retained-ID markers without walking live state or individual archive directories.
- Inspection returns at most 256 KiB of log tails per record.
- Archived specifications use the ordinary secret-omitting JSON contract;
  internal callback tokens never enter metadata, indexes, or log responses.

# Work Guidance

- Keep archive publication ordered so metadata and logs exist before index and ID marker visibility.

# Verification

- Unit tests cover archive/list/inspect, direct retained-ID checks, bounded pagination, and bucket cleanup.

# Child DOX Index
