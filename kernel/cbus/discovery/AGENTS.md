Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Discover package-owned command manifests and atomically publish their live
  command-bus registrations.

# Local Contracts

- Only ready active packages are indexed. TOML on the shared package mount is
  the source of truth; assembled catalogs are process-local and non-durable.
- `Reindex(ctx, packageIDs...)` reads only selected declaration folders; omitted
  IDs rebuild all ready packages. Cached unselected fragments and diagnostics
  survive; selected deletions remove fragments. Full-catalog collision checks
  run against cached fragments before atomic publication.
- Package commands are flat `cbus/commands/*.toml` declarations. Each requires a
  `command` containing the complete public name, such as
  `packages.repository.checkout`. No package prefix is inferred; filenames,
  including dotfiles, do not select names. Names are dot-separated lowercase
  kebab-case segments; `kernel` and `kernel.*` remain reserved.
- `program` names one same-package ordinary program. Version, help, examples,
  mutation/restart metadata, and secure inputs keep their existing contracts.
  Declarations are strict, size-bounded TOML. Shared package declaration
  discovery rejects nested directories, symlinks, and non-TOML files.
- Opaque IDs use the declaring package, active commit, and explicit command
  name. Filename-only renames preserve identity. Duplicate names within a
  package or across packages invalidate the entire conflicting package
  fragments.
- Package fragments are validated independently. A broken fragment is omitted
  without hiding valid packages.
- Candidate command manifests and their same-package programs are validated
  before package activation switches source.
- Dispatch package commands through the program runner as ordinary system-user
  jobs using normal shared package mounts. Discovery owns no execution policy.
- Preserve structured supervisor errors returned by the shared job system as
  command code/message/details; other failures use the runtime error code.

# Verification

- Tests cover explicit names, filename independence, rejected nesting/symlinks,
  malformed manifests, reserved names, same/cross-package duplicates,
  active-package filtering, candidate validation, and scoped atomic refresh.
- The rootless backend E2E test verifies discovered command dispatch through
  ordinary jobs with system identity and cross-package imports.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
