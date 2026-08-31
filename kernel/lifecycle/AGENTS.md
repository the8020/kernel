# Purpose

- Decouple graceful shutdown requests from application composition.

# Ownership

- Own one first-request-wins shutdown/restart state, its notification channel,
  and synchronized fixed-stage progress snapshots.
- Do not stop services or handle signals.

# Local Contracts

- Public API: `Manager`, `Progress`, `New`, `ConfigureShutdown`, `Request`,
  `RequestRestart`, `RestartRequested`, `Done`, `StartStep`, `CompleteStep`, and
  `Snapshot`.
- Closing `Done` happens exactly once and is safe from concurrent callers.
- The first shutdown or restart request selects the terminal action; repeated or
  competing requests cannot change it.
- Completion accounting is idempotent and bounded by the configured stage
  total. When parallel work completes, the latest still-active step remains
  visible instead of being hidden by a completed branch.
- Extend only when process-wide lifecycle state is currently required.

# Work Guidance

- Keep this package independent of application and command packages.

# Verification

- `lifecycle_test.go` race-checks concurrent, idempotent, bounded progress,
  active-step visibility, and first-request action selection; command-bus
  integration tests exercise lifecycle notification.

# Child DOX Index
