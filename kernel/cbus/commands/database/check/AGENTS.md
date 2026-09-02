# Purpose

- Implement the read-only database connectivity check.

# Local Contracts

- A failed check returns `database_unavailable` with backend and location
  details so startup wrappers can report it without stopping the kernel.
