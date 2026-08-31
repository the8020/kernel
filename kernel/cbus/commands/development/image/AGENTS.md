# Purpose

- Expose development-image status.

# Ownership

- Own the one declarative status leaf and its thin handler.

# Local Contracts

- The command delegates to the development service and returns digest, build
  time, Codex version, Deno version, and build status. Image construction is an
  installation concern and is not reachable through the running kernel.

# Work Guidance

- Keep image construction in `defaults/config/runtime/development/` and
  `install.sh`.

# Verification

- Generator and real development E2E gates cover image identity and tools.

# Child DOX Index
