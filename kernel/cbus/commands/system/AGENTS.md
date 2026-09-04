# Purpose

- Group process-level system commands.

# Ownership

- Own only `kernel.status`, `kernel.restart`, `kernel.shutdown`, and
  `kernel.reindex`.

# Local Contracts

- Status reads typed services; restart and shutdown request lifecycle
  notification without orchestrating cleanup in handlers. Reindex delegates to
  the package command indexer and returns its diagnostics.

# Work Guidance

- Keep process orchestration in `kernel/app`, not handlers.

# Verification

- Application and command tests verify all four commands, reindexing, cleanup,
  and restart action selection.

# Child DOX Index

- `status/AGENTS.md`: status result assembly.
- `restart/AGENTS.md`: lifecycle restart request.
- `shutdown/AGENTS.md`: lifecycle shutdown request.
- `reindex/AGENTS.md`: process-local package-command refresh.
