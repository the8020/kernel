Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Select a compatible sandbox from an immutable candidate snapshot.

# Ownership

- Own placement/sharing keys, grouping strategies, explicit overrides,
  runtime-profile compatibility, workload isolation, and Worker-capacity checks.
- A placement label may identify several sandboxes. It has no generated ID,
  lifecycle, state directory, or lookup alias.

# Local Contracts

- Public API: `Request`, `Candidate`, `Selection`, and `Select`.
- `isolated`, `owner`, `namespace`, and `shared` strategies apply to services and
  jobs, with defaults supplied by settings. Exact placement overrides, including
  empty labels, take precedence.
- `shared` and an explicit empty placement label select the same default key.
  Jobs, modules, and package programs use this group by default; their origins
  do not create separate sandbox classes.
- Joining requires the same workload type, placement/sharing key, full runtime
  profile, healthy ready/active state, and enough Worker capacity.
- A sandbox cannot contain two allocations of the same logical service.
- CPU and RAM are observations, not placement or admission limits.
- Selection returns the actual `SandboxID`; it creates no resource or identity.

# Work Guidance

- Keep selection pure. Sandbox creation and ownership belong to the coordinator
  and sandbox manager; clean warm accounting and provisioning belong to `pool`.

# Verification

- Unit tests cover strategies, overrides, profile splits, workload isolation,
  duplicate service rejection, and Worker-capacity exclusion.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
