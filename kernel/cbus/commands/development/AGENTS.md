# Purpose

- Retain development implementation adapters for private runtime operations.

# Ownership

- Do not publish CBus metadata; `the8020/dev-core` owns visible `dev-core.*`
  command programs.

# Local Contracts

- Every command delegates to `services.Development`; no command implements a
  second sandbox, Git, or activation path. Sandbox lifecycle and activation
  commands address the one sandbox by `user_id`.
- Destructive source and factory operations require an explicit `--confirm`.
- `development sandbox shell --command` executes through the kernel-owned
  sandbox driver and therefore remains a typed command-bus operation.

# Work Guidance

- Keep option conversion in the shared handler helper and domain
  behavior in `kernel/development`.

# Verification

- Handler, package, and development-domain tests cover arguments, delegation,
  and results.

# Child DOX Index

- `shared/`: shared thin-handler argument and result shaping.
- `image/`: read-only development image status.
- `sandbox/`: development sandbox lifecycle, shell, and reset commands.
- `activate/`: activation preview and execution commands.
