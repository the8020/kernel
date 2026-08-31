# Purpose

- Group process-level system commands.

# Ownership

- Own only the status, graceful restart, and graceful shutdown command packages.

# Local Contracts

- Status reads typed services; restart and shutdown request lifecycle
  notification without orchestrating cleanup in handlers.

# Work Guidance

- Keep process orchestration in `kernel/app`, not handlers.

# Verification

- Application integration verifies all three commands, cleanup, and restart
  action selection.

# Child DOX Index

- `status/AGENTS.md`: status result assembly.
- `restart/AGENTS.md`: lifecycle restart request.
- `shutdown/AGENTS.md`: lifecycle shutdown request.
