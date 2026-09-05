Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Convert generic workload requests into compatible existing or newly created
  runtime groups.

# Ownership

- Apply the shared grouping selector, enumerate healthy groups, reserve
  compatible clean warm capacity when available, generate secure
  sandbox/runtime-group identities and tokens, construct one typed sandbox
  specification, and invoke cold sandbox creation when needed.
- Do not schedule Workers, maintain workload registries, provision warm-pool
  processes, or implement backend/network details.

# Local Contracts

- Public API: `New`, `Coordinator.Ensure`, `Coordinator.Release`, and `Request`.
- Service and job requests use the same path. Matching keys reuse only healthy
  profile-compatible same-type groups and add a new shared owner; otherwise a
  clean matching warm sandbox is assigned or a fresh sandbox is created.
- Logical owner identity selects a grouping key; an optional allocation ID is
  the independent lifecycle claim stored on that sandbox. Services normally use
  one identity for both, while every job Worker uses its own claim so concurrent
  same-owner jobs can release independently.
- Cold construction requests its compact collision-checked sandbox ID from the
  sandbox manager, creates a compact `rgp-` runtime-group ID, and retains a
  generic opaque security token.
- Supplied profiles, mounts, permissions, dependency mode, global egress policy,
  resource limits, and lifecycle policy remain the authoritative compatibility
  and boundary inputs.
- Service placement supplies exactly one sandbox-group string and logical
  service ID. Reuse requires the same group/profile and refuses a sandbox that
  already contains that logical service; releasing the final owner delegates
  sandbox destruction to the manager.
- Existing service sandboxes are eligible only while their observed Worker count
  remains below the kernel-wide limit. If every compatible sandbox is full or
  already contains the service, cold construction retains the requested sandbox
  group. CPU and RAM observations never influence selection.

# Work Guidance

- Keep generated IDs opaque and never derive security tokens from owner/group
  values.

# Verification

- Unit tests cover same-owner reuse, cross-owner separation, persistent
  multi-owner shared groups, explicit shared keys, incompatible profiles,
  no-cross-type reuse, Worker-count capacity exclusion, placement-group
  retention, and new sandbox construction.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
