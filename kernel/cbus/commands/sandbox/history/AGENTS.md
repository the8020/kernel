Parent DOX: [kernel/kernel/cbus/commands/sandbox DOX](../AGENTS.md).

# Purpose

- Expose explicitly requested terminal sandbox history without mixing it into
  live sandbox inventory.

# Ownership

- Own bounded history listing and direct history-record inspection commands.
- Do not own archival, retention cleanup, or live lifecycle commands.

# Local Contracts

- `sandbox history list` is bounded and cursor-paginated.
- `sandbox history inspect` loads one immutable metadata record by history ID.
  Its status retains the node, creation time and log position; `kernel.logs`
  reads bounded pages using that reference and sandbox ID.

# Work Guidance

- A history ID addresses the retained record; its sbx-* ID addresses the actual
  sandbox. Log retention and metadata retention remain independently owned.

# Verification

- Generated validation and handler tests cover both commands.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.

- `list/` owns the bounded history inventory command.
- `inspect/` owns direct metadata and log-reference inspection.
