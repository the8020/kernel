# Purpose

- Define immutable sandbox/runtime-profile inputs and observable sandbox state
  without host side effects.

# Ownership

- Own workload/dependency/state enums, permissions, mounts, resources,
  network/control-endpoint/profile/spec/status models, validation, compatibility
  hashes, and transition rules.

# Local Contracts

- Public API: exported model types, `RuntimeProfile.Hash`,
  `SandboxSpec.Validate`, `ValidTransition`, `NewID`, `NewSandboxID`,
  `IsSandboxID`, `NewRuntimeGroupID`, and `NewWorkerID`.
- New sandbox, runtime-group, and Worker IDs are respectively `sbx-`, `rgp-`,
  and `wrk-` plus eight uniformly random lowercase alphanumeric characters;
  other opaque identities retain the generic format.
- Profile hashes include workload type, image digest, dependency mode,
  permission envelope, mounts, network mode, global egress allowance, Deno
  flags, and resource class; a profile cannot carry egress hosts when egress is
  disabled.
- A spec cannot mix workload types or owners, its mounts and permission envelope
  must exactly match its immutable runtime profile, and image identity must be a
  SHA-256 digest.
- Service specs retain one exact placement-group value plus the logical service
  IDs already present. The lists are used to prevent duplicate allocations in one
  sandbox; an empty placement group remains a valid shared value.
- Sandbox resource limits contain only PID and temporary-filesystem bounds;
  CPU and RAM fields are observations, not limits or placement inputs.

# Lifecycle

- Models are constructed and validated before persistence or backend calls;
  states follow only the declared transition graph.

# Failure Behavior

- Invalid identity, limits, internal/control ports, permissions, profile
  mismatch, or transition returns a descriptive error before host mutation.

# Concurrency

- Values are immutable by convention; callers copy slices/maps before sharing
  and managers synchronize state mutation.

# Dependencies

- Go standard library only.

# Non-Responsibilities

- No filesystem access, containerd calls, CNI, cgroups, supervisor requests, or
  scheduling.

# Verification

- Unit tests cover validation, deterministic hashing/canonicalization including
  egress policy, workload isolation, sandbox capacity policy, generic IDs,
  compact runtime resource IDs, strict sandbox-ID recognition, and every
  legal/illegal state transition.

# Child DOX Index
