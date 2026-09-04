# Purpose

- Expose live sandbox inventory, inspection, resources, lifecycle, and separate
  terminal history commands.

# Ownership

- Own declarative sandbox list/inspect/metrics/stop/kill/delete and bounded
  history list/inspect handlers and delegate to the sandbox manager.

# Local Contracts

- `sandbox list` exposes only sandbox ID, workload type, observed state, concise
  reason, Worker count, warm status, runtime-group ID, and any failure;
  `sandbox inspect` owns the complete specification, status, Workers, resources,
  leases, and correlated services. Application-owned logical state
  is never discovered or duplicated in sandbox inspection.
- Assigned service sandboxes use only their first sorted logical service ID as
  the concise reason, formatted as `service:<service-id>`; list and inspection
  must not expose every colocated service or an opaque placement-group key.
- `sandbox history list` is the only archive inventory path and returns a
  bounded cursor page; `sandbox history inspect` directly loads immutable
  terminal metadata and bounded log tails by history ID.
- Stop is graceful, kill is immediate, and delete performs complete
  manager-owned cleanup, removes warm-pool accounting, and triggers replacement
  capacity when required.

# Work Guidance

- Accept either sandbox or runtime-group identity where the manager supports it.

# Verification

- Generated validation and handler tests cover every lifecycle command.

# Child DOX Index

- `history/AGENTS.md`: bounded terminal sandbox history listing and inspection.
- This domain contract directly owns the remaining leaf lifecycle command
  folders.
