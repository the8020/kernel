Parent DOX: [kernel/kernel/cbus/commands/database DOX](../AGENTS.md).

# Purpose

- Implement the read-only database connectivity check.

# Local Contracts

- A failed check returns `database_unavailable` with backend and location
  details so startup wrappers can report it without stopping the kernel.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
