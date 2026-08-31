# Purpose

- Expose development-workspace sandbox lifecycle, shell, and reset operations.

# Ownership

- Own declarative create/list/inspect/start/stop/restart/kill/delete/shell,
  source-reset, and factory-reset leaves.

# Local Contracts

- Source and factory reset require explicit confirmation; delete explicitly
  chooses whether developer home is retained. Shell execution remains inside
  the selected gVisor sandbox.
- Lifecycle commands never flush, scan, or snapshot content; source, home, and
  system files are already native durable storage.

# Work Guidance

- Keep lifecycle and filesystem behavior in `kernel/development`.

# Verification

- Domain tests and the real rootless E2E cover lifecycle, shell edits, persistence,
  and reset distinctions.

# Child DOX Index
