Parent DOX: [system commands](../AGENTS.md).

# Purpose

- Implement `kernel.update <major.minor.patch>` as a background script launch.

# Ownership

- Validate the version's shape, require the installed updater and live launcher,
  detach with a new process session, and return PID and private log path.
- The installed [host scripts](../../../../../defaults/kernel-scripts/AGENTS.md)
  own release validation, locking, source preparation, shutdown, migrations,
  and restart.

# Local Contracts

- Request cancellation and kernel shutdown do not cancel the updater process.
  Reap the child while this kernel remains alive. No new runtime service or
  package execution path participates in its lifetime.
- The result acknowledges process launch; eventual success or failure belongs
  to `node/kernel/update.log` and `node/kernel/update/state.json`.

# Work Guidance

- Keep this adapter small; reuse the installed script without implementing
  migration or release-selection policy in Go.

# Verification

- `go test ./kernel/cbus/commands/system/update` exercises validation, missing
  installation/launcher, detached execution, cancellation independence, and logs.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
