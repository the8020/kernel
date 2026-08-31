# Purpose

- Expose direct filesystem package discovery, desired package management, Git
  synchronization, and independent development-repository administration
  through the administrative command bus.

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
- Package commands remain available even when the Deno runtime is degraded.
- Index list/inspect/set manage kernel-owned desired package records. Source
  inspection lists bounded public refs without cloning; version listing fetches
  and reports bounded commit history. Synchronization accepts one, several, or
  all indexed packages and reports each result independently.
- A changed synchronized package retires removed service capacity and increments
  the generations of its current services so active Workers reload. Offline
  dispatch performs only the package transaction because no runtime exists.
- Local creation writes a minimal valid manifest, initializes an independent
  Git repository and first commit, and records a source-free local index entry.
- Repository initialization is explicit, never inferred from discovery, and
  creates one initial commit at the package root. Remote configuration is
  informational for activation and does not authorize a push.

# Work Guidance

- Return validation failures as package data so one invalid package does not
  hide valid siblings.

# Verification

- Generator catalog and aggregate handler tests cover discovery, management,
  and repository commands; package-store tests own discovery/path safety and
  real Git synchronization. Package-command tests cover offline synchronization,
  changed-service reload and retirement, and refresh failure reporting, while
  development-domain tests own activation histories.

# Child DOX Index

- This domain contract owns the `list`, `inspect`, `index`, `source`, `version`,
  `synchronize`, and `local` leaves.
- `repository/AGENTS.md`: independent package Git inspection, initialization,
  remote configuration, and status.
