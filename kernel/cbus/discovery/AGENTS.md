Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Discover package-owned command manifests and atomically publish their live
  command-bus registrations.

# Local Contracts

- Only ready active packages are indexed. TOML on the shared package mount is
  the source of truth; assembled catalogs are process-local and non-durable.
- `Reindex(ctx, packageIDs...)` reads only selected declaration folders; omitted
  IDs rebuild all ready packages. Cached unselected fragments and diagnostics
  survive; selected deletions remove fragments. Cached declarations also refresh
  when their target program package changes, including recovery after a missing
  target returns. Full-catalog collision checks precede atomic publication.
- Package commands are flat `cbus/commands/*.toml` declarations. Each requires a
  `command` containing the complete public name, such as
  `packages.repository.checkout`. No package prefix is inferred; filenames,
  including dotfiles, do not select names. Names are dot-separated lowercase
  kebab-case segments; `kernel` and `kernel.*` remain reserved.
- `program` requires one full `namespace/package/program` ID, shared with hooks
  and events. Short names are invalid; the target may be in another package.
  Dispatch checks the target package commit while command identity and origin
  remain with the declaring package. Version, help, examples, mutation/restart
  metadata, and secure inputs keep their existing contracts. Declarations are
  strict, size-bounded TOML. Shared package declaration discovery rejects nested
  directories and symlinks; non-TOML regular files such as `AGENTS.md` are
  ignored.
- Opaque IDs use the declaring package, active commit, and explicit command
  name. Filename-only renames preserve identity. Duplicate names within a
  package or across packages invalidate the entire conflicting package
  fragments.
- Package fragments are validated independently. A broken fragment is omitted
  without hiding valid packages.
- The transaction owner inspects declarations outside the catalog lock and
  rejects stale publication snapshots. Ordinary discovery tests cover concurrent
  refresh.
- Candidate command manifests and program references resolve through the shared
  package resolver against the entire candidate batch, then ready active
  packages, before activation switches source. Removing a referenced program or
  target package fails validation, including references from unchanged owners.
- Empty-commit removal candidates omit their command fragment from candidate
  validation; publication uses the existing scoped reindex to remove it.
- Dispatch package commands through the program runner as ordinary system-user
  jobs using normal shared package mounts. Discovery owns no execution policy.
- Return the program's result without collecting console messages into the
  command response. Project its allocated IDs, saved log position and times into
  the command's execution reference on both success and failure. No new identity
  is generated here; console messages use the ordinary unified logging path.
- Preserve structured supervisor errors returned by the shared job system as
  command code/message/details; other failures use the runtime error code.

# Verification

- Tests cover explicit names, filename independence, rejected nesting/symlinks,
  malformed manifests, reserved names, same/cross-package duplicates,
  active-package filtering, cross-package dispatch/commit checks, candidate
  resolution/deletion, and scoped atomic refresh with immutable old handlers.
- The rootless backend E2E test verifies discovered command dispatch through
  ordinary jobs with system identity, a program in another package, and
  cross-package imports.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
