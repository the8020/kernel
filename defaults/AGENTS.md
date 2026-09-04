# Purpose

- Hold source-owned inputs used to create a fresh 80|20 instance.

# Ownership

- `bootstrap-packages.toml` owns the small initial package source list used only
  when a database has no initialized package set.
- `config/runtime/` owns platform-maintained runtime versions, protocol source,
  Deno supervisor/Worker source, generic SDKs, and service/development image
  construction.
- `scripts/` owns platform-maintained helpers mounted read-only and executable
  in development sandboxes.
- Defaults contain no node identity, credentials, users, shared settings,
  operational state, package source, or materialized images.

# Local Contracts

- `install.sh`, not kernel startup, atomically refreshes runtime definitions into
  `node/kernel/runtime/definitions/` and the complete instance `scripts/` tree.
- On a fresh fixed-layout instance, installation stages every bootstrap package
  under `packages/`. Local development sources become clean deterministic Git
  snapshots; remote sources retain their repository history.
- First kernel boot publishes those staged packages only after one batched table
  evaluation/synchronization and activation-hook run. Existing databases and
  ordinary restarts never reapply the bootstrap list or rescan all tables.
- Materialized images, build caches, downloads, smoke records, and temporary
  construction output belong under `node/kernel/runtime/` or source
  `.development/`, never in this tree.

# Work Guidance

- Keep bootstrap inputs minimal, deterministic, package-neutral, and free of
  secrets.

# Verification

- Installation and instance tests verify fresh package staging, runtime/source
  refresh, fixed roots, and node-local artifact placement.

# Child DOX Index

- `config/runtime/AGENTS.md`: canonical generic runtime source, image
  definitions, materialization, and verification.
