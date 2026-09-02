# Purpose

- Store the authoritative modular TOML definitions compiled into the kernel.

# Ownership

- Own keys, types, required node/global storage, defaults, environment
  variables, constraints, runtime/restart metadata, and descriptions for
  80|20 settings.
- `network/` owns node-local main/SSH ports and the global root alias;
  `logging/` owns kernel logging; `sandbox/` owns backend, network, resources,
  history, startup/shutdown, PID/tmpfs resources, and debugging; `runtime/` owns
  generic supervisor timing, node count/storage admission budgets, and the
  kernel-wide sandbox Worker capacity; `execution/`, `service/`, `services/`, and `job/` own generic
  grouping, canonical service defaults, reconciliation, and job policy; and
  `auth/` owns bootstrap authentication-session, cookie, and Argon2id settings;
  `database/` owns the global backend, location, and plain development-phase
  PostgreSQL credentials plus the node-local live connection-pool limits.
- No definition subtree or setting may describe an application protocol,
  application program, application state schema, or UUI behavior.

# Local Contracts

- Every `.toml` file defines exactly one setting and is recursively discovered
  by `kernel/cbus/gen`.
- Every file declares `storage = "node"` or `storage = "global"`; storage never
  falls back implicitly. String-format constraints use declarative `pattern`
  metadata rather than owner-specific settings parsing.
- Keys and environment variables are unique; values must pass generated-catalog
  validation.
- Files are data, not executable hooks.

# Work Guidance

- Add no setting without a current kernel owner and acceptance requirement.

# Verification

- Generator and settings tests validate discovery, duplicates, types, mandatory
  storage, defaults, constraints, and runtime metadata.

# Child DOX Index
