# Purpose

- Own filesystem-derived package and service definitions, selected-package
  content inspection, the shared desired package index and Git synchronization,
  and the replaceable shared service desired-state boundary.

# Ownership

- Collection discovery reads only
  `packages/<namespace>/<repository>/package.toml` and each package's
  `services/<service>/service.toml`.
- Selected-package inspection reads fixed-depth service and program manifests
  plus a bounded recursive non-Git file inventory. It reports portable metadata
  without reading or materializing mutable service desired state; Deno retains
  ownership of program discovery, loading, and execution.
- Derive package IDs, service IDs, canonical HTTP prefixes, and source paths
  exclusively from validated filesystem segments.
- Parse and validate package, canonical service lifecycle/scaling/placement and
  access policy, and desired-state TOML; calculate effective configuration from
  framework defaults, portable defaults, then state overrides.
- Own `ServiceStateStore` and the `FileServiceStateStore` backend under
  `state/services/.../state.toml`, with per-service advisory locks, file flush,
  atomic rename, fixed-depth listing, and idempotent deletion.
- Materialize framework and portable defaults into the first environment state
  record so later package-default edits cannot silently change deployed
  behavior.
- Own fixed-depth `state/package-index/<author>/<repository>.toml` documents,
  public HTTPS Git source/ref inspection, bounded version history, atomic
  package worktree replacement, clean-worktree protection, and initial local
  package repository creation.
- Emit structured package/service discovery and validation events when a logger
  is supplied.
- Do not own runtime Workers, reconciliation, HTTP dispatch, application routes,
  or node-local observed state.

# Local Contracts

- Public API: `New`, summary discovery and selected-package inspection methods,
  package-index list/inspect/set, source inspection, version listing,
  synchronization and local creation,
  `ReadService`, `ReadState`, `MutateState`, `ServiceStateStore`,
  `FileServiceStateStore`, identity/path types, manifest types,
  `FrameworkDefaults`, and effective configuration types; logging is optional
  and does not alter results.
- Names match `^[A-Za-z0-9][A-Za-z0-9._-]*$`; hidden names, traversal,
  separators, null bytes, symlink escapes, and entrypoints outside the package
  tree are rejected.
- First discovery creates generation-zero desired state using
  `lifecycle.default_enabled` and frozen effective defaults; ordinary services
  default disabled.
- Omitted service and access types normalize to `stateless` and `public`;
  explicit service types are only `stateless` or `session`, independent of
  HTTP/WebSocket transport. Session keepalive remains editable and positive for
  both types so switching type never loses its configured value.
- Canonical scaling owns non-negative minimum/maximum Workers, positive
  concurrency per Worker, target utilization in `(0,1]`, and positive Worker
  keepalive. Maximum zero means unlimited only at the service level; otherwise
  it cannot be below minimum. Canonical placement owns one trimmed optional
  sandbox group, non-negative minimum sandboxes, and positive Workers per
  sandbox. Defaults are zero/zero Workers, concurrency 32, target `0.7`, Worker
  keepalive two minutes, zero warm sandboxes, four Workers per sandbox,
  stateless type, and ten-minute session keepalive.
- Service manifest and desired-state schema 2 are the only accepted format.
  Older schemas and obsolete fields are rejected; development instances are
  reinitialized instead of migrated.
- Framework defaults are defined once by `DefaultFrameworkDefaults`; callers may
  supply setting-derived defaults through `Config`.
- Package content inspection excludes `.git`, does not follow directory
  symlinks, returns package-relative file paths, and stops after 5,000 visited
  entries with an explicit truncation marker.
- Package index identity is repeated in each schema-one document and must match
  its fixed-depth path and public source suffix. A remote entry selects exactly
  one of latest default branch, hexadecimal commit, or safe Git tag; a local
  entry has no source or selector.
- Synchronization clones into a hidden same-filesystem staging directory,
  validates `package.toml`, resolves an exact commit, and switches the complete
  repository by rename. Existing repositories with tracked or untracked changes
  are preserved and rejected. One failed package does not prevent other
  selected packages from synchronizing.

# Work Guidance

- Keep collection scans fixed-depth; recursive traversal is permitted only for
  bounded inspection of one explicitly selected package. Do not add recursive
  catalog discovery or a persistent definition cache.
- Return invalid discovered entries with precise path-scoped validation errors
  instead of aborting the complete scan.

# Verification

- Tests cover cheap fixed-depth package summaries, selected package
  service/program/file inspection without desired-state materialization,
  filesystem identity, descriptions, invalid and hidden names, symlink escape,
  service entrypoints, schema-two defaults/validation and obsolete-schema
  rejection, frozen first-discovery state,
  backend replacement, CRUD/list/delete behavior, monotonic atomic
  mutations, cross-process advisory locking, index validation and permissions,
  real HTTPS Git ref/version discovery, latest/tag synchronization, service-set
  changes, dirty-worktree preservation, and local repository creation.

# Child DOX Index
