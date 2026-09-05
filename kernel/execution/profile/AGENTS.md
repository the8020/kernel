Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Derive a compatible parent runtime profile from an explicitly requested Worker
  permission set.

# Ownership

- Keep filesystem permissions within existing mounts, add an explicitly
  requested development workspace only through the sandbox mount policy,
  validate bounded host/environment permission syntax, expand the parent
  network/import/environment/system envelope, and select online dependency mode
  when remote imports are explicitly requested.
- Do not supply environment values, configure CNI/firewalls, or execute Workers.

# Local Contracts

- Public API: `ForWorker`, `ForWorkerWithWorkspace`, `MountPolicy`, and
  `Workspace`.
- Worker read/write permissions may narrow but never broaden the mounted parent
  filesystem envelope. Network/import permissions require the global profile
  egress policy and become compatibility inputs; infrastructure environment
  variables, including supervisor endpoints and dependency mode, remain
  forbidden to program Workers.
- A development workspace is mounted at `/workspace`, is read-only unless
  explicitly writable, carries owner scope in the compatibility hash, and is
  unavailable unless a host mount policy approves its source.

# Work Guidance

- Treat every derived profile as immutable and let its canonical hash split
  incompatible runtime groups.
- Every derived profile must own independent mount, permission, and startup-flag
  slices. Never let append capacity or element mutation alias the parent or a
  sibling profile.

# Verification

- Unit tests cover narrowing, policy-approved owner-scoped workspaces, read-only
  enforcement, network/import profile splitting, disabled egress, online mode,
  system permission, filesystem escape, unsafe hosts, and reserved environment
  denial.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
