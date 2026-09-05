Parent DOX: [kernel/defaults DOX](../AGENTS.md).

# Purpose

- Provide platform-owned helpers installed into development sandboxes.

# Ownership

- Own `activate`, `activate.ts`, `install-codex.sh`, and `install-claude.sh`;
  installation refreshes the instance scripts tree.

# Local Contracts

- Helpers are mounted read-only at `/workspace/scripts`; activation uses the
  typed kernel ingress and requires a publication message.
- Agent installers are opt-in, target persistent sandbox root storage, and
  configure only unattended full-access behavior.

# Work Guidance

- Keep bootstrap inputs minimal and avoid changing unrelated user settings.

# Verification

- Kernel development unit tests exercise installers with isolated homes and
  upstream installer doubles.
- Existing development and activation tests verify the shared activation path.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
