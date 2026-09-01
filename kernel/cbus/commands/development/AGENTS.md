# Purpose

- Expose development images, persistent user sandboxes, and activation through the
  existing declarative command bus.

# Ownership

- Own the `development image status`, `development sandbox`, and
  `development activate` command definitions and thin handlers.
- Package repository commands remain under the existing `package` command
  domain.

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

- Generator tests cover the declarative catalog; handler and development-domain
  tests cover arguments, delegation, and results in one-shot and interactive
  clients.

# Child DOX Index

- `shared/`: shared thin-handler argument and result shaping.
- `image/`: read-only development image status.
- `sandbox/`: development sandbox lifecycle, shell, and reset commands.
- `activate/`: activation preview and execution commands.
