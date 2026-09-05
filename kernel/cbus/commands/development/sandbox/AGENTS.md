Parent DOX: [kernel/kernel/cbus/commands/development DOX](../AGENTS.md).

# Purpose

- Expose per-user development sandbox lifecycle, shell, and reset operations.

# Ownership

- Own declarative create/list/inspect/start/stop/restart/kill/delete/shell,
  source-reset, and factory-reset leaves.

# Local Contracts

- Every command addresses the one sandbox by `user_id`. Source and factory reset
  require explicit confirmation; delete removes exactly that user's
  `dev-sandbox` root. Shell execution remains inside the selected gVisor
  sandbox.
- Lifecycle commands delegate checkpoint and storage behavior to the development
  manager.

# Work Guidance

- Keep lifecycle and filesystem behavior in `kernel/development`.

# Verification

- Domain tests and the real rootless E2E cover lifecycle, shell edits,
  persistence, and reset distinctions.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
