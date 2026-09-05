Parent DOX: [kernel/kernel/cbus/commands/system DOX](../AGENTS.md).

# Purpose

- Implement `kernel.reindex` as declared by the adjacent authoritative TOML.

# Ownership

- Delegate explicit refresh of the process-local command, event-handler, and
  hook-handler indexes plus package-scoped service fragments to runtime
  composition.

# Local Contracts

- `kernel.reindex` rebuilds all active packages. Optional `--packages` accepts
  comma-separated full package IDs and replaces only their declarations.
- Results retain command revision, packages, command count, and diagnostics,
  plus total `events` and `hooks` counts. Invalid handler declarations reject
  handler publication; invalid command fragments retain independent diagnostics.
  `indexed_packages` lists accepted service fragments and `service_diagnostics`
  explains publication/runtime failures. Failed calls preserve this result in
  structured error details and expose the full publication message, including
  which updates were not applied, instead of an internal-kernel-error
  placeholder.

# Verification

- Discovery, package, composition, and command tests cover full/scoped refresh,
  deleted declarations, target-commit changes, diagnostics, and lifecycle
  wiring.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
