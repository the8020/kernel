# Purpose

- Group process-level system commands.

# Ownership

- Own only `kernel.status`, `kernel.restart`, `kernel.shutdown`, and
  `kernel.reindex`, `kernel.events.emit`, and `kernel.signing.status/replace`.

# Local Contracts

- Status reads typed services; restart and shutdown request lifecycle
  notification without orchestrating cleanup in handlers. Reindex delegates to
  the shared package indexer, accepts optional package IDs, and returns command
  diagnostics, event/hook counts, and accepted service fragments. Publication
  failures retain actionable messages and partial-publication details. Event emission parses
  JSON data and delegates asynchronous admission to the shared event dispatcher.

# Work Guidance

- Keep process orchestration in `kernel/app`, not handlers.

# Verification

- Application and command tests verify lifecycle commands, event emission, reindexing, cleanup,
  and restart action selection.

# Child DOX Index

- `signing/AGENTS.md`: private deployment-key administration.

- `status/AGENTS.md`: status result assembly.
- `restart/AGENTS.md`: lifecycle restart request.
- `shutdown/AGENTS.md`: lifecycle shutdown request.
- `reindex/AGENTS.md`: native declarations and package-scoped service-fragment refresh.
- `emit/AGENTS.md`: local asynchronous event emission with JSON data.
