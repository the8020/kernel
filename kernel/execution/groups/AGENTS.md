# Purpose

- Select compatible runtime groups generically and account for clean warm groups by runtime-profile hash.

# Ownership

- Own grouping strategies, explicit overrides, stable logical group keys, compatibility matching, no-cross-type enforcement, warm reservation/assignment/destruction accounting, and replenishment demand.

# Local Contracts

- Public API: `Request`, `Group`, `Selection`, `Select`, `WarmPool`, `NewWarmPool`, and pool mutation/restoration/snapshot methods.
- `isolated`, `owner`, `namespace`, and `shared` strategies apply identically to user/service/job workloads; defaults are supplied from settings.
- Joining requires workload type, group key, and complete runtime-profile hash compatibility.
- Service groups additionally require the same exact placement-group value and
  reject candidates already containing the logical service. Empty placement
  group is the ordinary shared default; there is no dedicated flag or tag list.
- Used warm supervisors are destroyed on release and never returned to the clean pool.

# Lifecycle

- Select existing compatible group or create/assign a clean warm group; reserve atomically, verify health externally, assign, then replenish asynchronously.

# Failure Behavior

- Missing owner/namespace/execution/profile, invalid strategy, cross-type candidate, or invalid pool transition returns a descriptive error without assignment.

# Concurrency

- Selection operates on immutable snapshots; `WarmPool` serializes all accounting and reservations with one mutex.

# Dependencies

- Go standard library and `sandbox/model`.

# Non-Responsibilities

- No sandbox creation, health checks, Worker lifecycle, service/job/session policy, or persistent state.

# Verification

- Unit tests cover every strategy and override, same-key profile splits, cross-type exclusion, defaults, concurrent reservations, resize, durable accounting restoration, failure, assignment, destruction, and replenishment counts.

# Child DOX Index
