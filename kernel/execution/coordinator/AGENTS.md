# Purpose

- Convert generic workload requests into compatible existing or newly created runtime groups.

# Ownership

- Apply the shared grouping selector, enumerate healthy groups, reserve compatible clean warm capacity when available, generate secure sandbox/runtime-group identities and tokens, construct one typed sandbox specification, and invoke cold sandbox creation when needed.
- Do not schedule Workers, maintain workload registries, provision warm-pool processes, or implement backend/network details.

# Local Contracts

- Public API: `New`, `Coordinator.Ensure`, `Coordinator.Release`, and `Request`.
- User, service, and job requests all use the same path. Matching keys reuse only healthy profile-compatible same-type groups and durably add a new shared owner; otherwise a clean matching warm sandbox is assigned or a fresh sandbox is created.
- Cold construction requests its compact collision-checked sandbox ID from the
  sandbox manager, creates a compact `rgp-` runtime-group ID, and retains a
  generic opaque security token.
- Supplied profiles, mounts, permissions, dependency mode, global egress policy, resource limits, and lifecycle policy remain the authoritative compatibility and boundary inputs.
- Service placement supplies exactly one sandbox-group string and logical
  service ID. Reuse requires the same group/profile and refuses a sandbox that
  already contains that logical service; releasing the final owner delegates
  sandbox destruction to the manager.

# Work Guidance

- Keep generated IDs opaque and never derive security tokens from owner/group values.

# Verification

- Unit tests cover same-owner reuse, cross-owner separation, persistent multi-owner shared groups, explicit shared keys, incompatible profiles, no cross-type reuse, and new sandbox construction.

# Child DOX Index
