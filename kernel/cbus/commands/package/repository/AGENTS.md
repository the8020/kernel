# Purpose

- Expose independent package Git-repository administration.

# Ownership

- Own list, inspect, explicit initialization, remote configuration, status,
  pull, push, and branch/commit checkout command leaves.

# Local Contracts

- Package IDs resolve to exactly `packages/<namespace>/<repository>/`.
  Initialization creates the package's first local commit; remote configuration
  never pushes and rejects embedded credentials. Pull is fast-forward only;
  checkout and pull reject dirty worktrees. Changed HEAD operations trigger the
  shared targeted package-service refresh. No root-level repository command
  exists.

# Work Guidance

- Keep repository, path, locking, and authentication behavior in
  `kernel/packages`; handlers only compose runtime refresh.

# Verification

- Package-store and command tests prove path safety, independent histories,
  local-only initialization, configured remotes, pull/push/checkout behavior,
  transient authentication, and targeted service refresh.

# Child DOX Index
