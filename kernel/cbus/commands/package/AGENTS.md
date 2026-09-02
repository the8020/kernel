# Purpose

- Expose direct filesystem package discovery, desired package management, Git
  synchronization, and package-repository administration through the
  administrative command bus.

# Ownership

- Own `package list`, `package inspect`, `package index`, `package source`,
  `package version`, `package synchronize`, `package local create`, and
  `package repository` declarative definitions and thin handlers.

# Local Contracts

- Every invocation delegates to the filesystem store and therefore reads current
  package manifests without a catalog cache.
- `package list` exposes only package identity, description, validity, service
  count, and any validation failure. `package inspect` owns filesystem paths,
  complete package metadata, fixed-depth service/program metadata, and the
  bounded non-Git file inventory for one selected package.
- Package commands remain visible when the Deno runtime is degraded. Mutations
  fail closed until the schema evaluator is installed, except for the deliberate
  pre-database offline first-install synchronization path.
- Index list/inspect/set manage kernel-owned desired package records. Source
  inspection lists bounded refs without cloning; version listing fetches
  and reports bounded commit history. Synchronization accepts one, several, or
  all indexed packages and reports only package ID, resolved commit, and
  success for each result; Git and service-refresh details remain internal.
- A changed synchronized package retires removed service capacity and increments
  the generations of its current services so active Workers reload. A completed
  generation switch remains a successful synchronization while occupied old
  Workers drain asynchronously. Offline dispatch performs only the package
  transaction because no runtime exists.
- Local creation writes a minimal valid manifest, initializes an independent
  Git repository and first commit, and records a source-free local index entry.
- Repository initialization is explicit, never inferred from discovery, and
  creates one initial commit at the package root. Remote configuration rejects
  embedded credentials. Pull fast-forwards a clean attached branch, push
  publishes it, and checkout selects a branch or detached commit. Pull and
  checkout refresh only services affected by a changed HEAD. A package index
  may select a global secret by name; handlers never resolve or return its
  value.

# Work Guidance

- Return validation failures as package data so one invalid package does not
  hide valid siblings.

# Verification

- Generator catalog and aggregate handler tests cover discovery, management,
  repository commands, and secret-name options; package-store tests own
  discovery/path safety and real Git synchronization/authentication.
  Package-command tests cover offline synchronization,
  changed-service reload and retirement, and refresh failure reporting, while
  development-domain tests own activation histories.

# Child DOX Index

- This domain contract owns the `list`, `inspect`, `index`, `source`, `version`,
  `synchronize`, and `local` leaves.
- `repository/AGENTS.md`: independent package Git inspection, initialization,
  remote configuration, status, pull, push, and checkout.
