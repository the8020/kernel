# Purpose

- Implement `kernel.reindex` as declared by the adjacent authoritative TOML.

# Ownership

- Own only explicit refresh of the process-local package command catalog.

# Local Contracts

- Reindex validates active packages independently and atomically publishes all
  valid fragments with diagnostics for omitted fragments.

# Verification

- Discovery and command-bus tests cover stale-catalog recovery and diagnostics.

# Child DOX Index
