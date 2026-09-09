Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own package source inspection, database-backed package identity, Git
  synchronization, and the atomic package activation transaction.

# Ownership

- Discover native package, program, command, table, hook, and event definitions
  only beneath validated `packages/<namespace>/<repository>/` roots.
- Own database adapters for `the8020/packages` desired/active package records,
  activation history, and hook phase checkpoints. Application service tables,
  declarations, defaults, and overrides belong exclusively to Deno services.
- Own package Git source/ref inspection, clean worktree staging/replacement,
  local repository initialization, package deletion, pull/push/checkout, and
  transient named-secret authentication.
- Coordinate candidate table evaluation/synchronization, pre/post activation
  hooks, source publication, durable recovery, and targeted service refresh.
- Do not own physical SQL generation, evaluator execution, runtime Workers,
  request routing, or node-local observed runtime state.

# Local Contracts

- Package and service IDs derive from validated filesystem segments. Selected
  package inspection is bounded; collection discovery is fixed-depth and never
  recursively scans every package in a hot or periodic path.
- Service runtime specifications come from the synchronous `index-services`
  chain. The kernel does not discover/parse service declarations, merge policy,
  persist application configuration, or issue service-specific revision SQL.
- Generic `IndexRevisionFollower` reads the system `indexes` scalar and newer
  `index:<package>` and `restart:<service>` markers, then invokes the same
  targeted reindex or generic restart entry point. Deno publishes those markers
  with desired configuration. No application table scan or parallel service
  revision poller is permitted.
- `PackageRevisionFollower` observes the shared `packages` revision and ready
  commits without fetching, cloning, or replacing sources on another node.
  Bounded Git diffs yield resolved sandbox file paths, including deletions and
  both sides of renames. All nodes read the same authoritative mutable checkout;
  commit metadata must remain available for the pending revision comparison.
- A removed package produces its sandbox directory with a trailing slash for
  observed-import matching. Removal does not require retaining its Git objects.
- On a source update, `ReactToSourceUpdate` requests an observed-import scan of
  current service Workers, deduplicates logical services, and requests a soft
  restart with that package revision as update identity. Import sets remain
  Worker-owned and jobs are never restarted or replayed. Unobserved late
  imports, stale caches, and failures in old work remain accepted shared-source
  tradeoffs.
- The database package record is the sole desired/active package source of truth
  after first initialization. Bootstrap TOML is only the fresh-database source
  list; ordinary boots trust database state and never rescan all definitions.
  Release-staged repositories carry their resolved requested tag in local Git
  metadata; bootstrap verifies that it names the evaluated commit, then records
  the tag as desired identity and the exact commit as active identity.
- A fresh database stages every bootstrap package and performs one batched
  evaluation and synchronization before publishing the package set. Normal
  activation evaluates only candidate package tables in one batch.
- Activation transactions use shared `act-` IDs. The initial database insert is
  exclusive, rejects collisions, and shares that identity across hook and
  activation history throughout recovery.
- Activation is prepare schema → pre-activate hooks → atomically switch source →
  publish package records → post-activate hooks → complete. Durable activation,
  package, hook, and pending-deployment rows make retry/recovery idempotent.
  PostgreSQL holds one database advisory deployment lock throughout; SQLite uses
  local transactional serialization for its single-node role.
- An activation candidate with an empty commit removes its package; its root is
  the installed path. Removal executes no activation handlers from deleted code,
  retires the package record and tables, removes its active catalog commit, and
  publishes one revision through the same recovery/reindex path. New candidates
  acquire local package records automatically when no desired record exists.
- `DeletePackage` rejects uncommitted work and unsafe package/namespace paths,
  stages installed source in the existing `.previous` activation backup, and
  removes that backup after publication. Uninstalled desired packages can also
  be deleted. Retained activation history and physical table data are preserved;
  reinstalling a retired package needs no manual index edit.
- Rollback restores previous ready package records before the schema evaluator
  rereads their still-active source.
- Removed table/column definitions become retired metadata and retained physical
  data. Only explicit confirmed trim performs destructive deletion. Incompatible
  type, nullability, key, index, or constraint changes reject activation.
- Handler folders are flat: `events/*.toml` requires `event`, `description`, and
  `program`; `hooks/*.toml` requires `hook`, `description`, and `program`. Hooks
  also accept integer `order`, defaulting to zero. Each trigger retains all
  declarations sorted by ascending order and full declaration identity.
  Filenames never select triggers. Program IDs are full
  `namespace/package/program` identifiers; ordinary program manifests own
  entrypoints. Unknown fields, missing/invalid triggers, descriptions or
  programs, nested folders, and symlinks fail validation. Loose source files
  never declare or execute handlers.
- `ReindexHandlers(ctx, packageIDs...)` atomically replaces both process-local
  handler indexes. Omission rebuilds all ready packages. A selection reads only
  selected declaration folders, removes deleted/inactive declarations, retains
  unselected fragments, and refreshes cached cross-package program references
  when their target changes. Invalid input leaves both indexes unchanged. Event
  lookup is memory-only and bounded to 2,048 listeners globally.
- Hooks accept `pre-activate`, `post-activate`, or `index-services`.
  `PackageHooks` selects one package's phase and `Hooks` selects an ordered
  trigger chain across packages; both consume only the published memory index.
  Activation phases use a validated candidate index before live publication.
  Program references resolve against all candidates before ready installed
  packages. Read-only candidate mounts override only roots that differ from
  installed package paths; bootstrap uses the existing shared mount without
  redundant overrides. Recovery rebuilds its candidate index and retains durable
  phase completion, never repeating successful phases.
- `RunHookChain` invokes `worker/hook_dispatch.ts` as one ordinary system job
  per trigger invocation. Service indexing passes all selected packages in one
  invocation; activation retains its durable per-package phase boundaries.
  Handlers receive the same mutable state and a separate frozen invocation
  scope. Activation scope contains the declaring package, previous/candidate
  commits, first-activation flag, and activation ID. Only initial input and
  final output cross the Worker boundary. Hook failures stop the chain and
  identify the failing declaration. Normal job permissions, mounts, grouping,
  and reuse apply; candidate mounts enter normal profile compatibility. Never
  copy packages or force per-handler isolation. Resolved chain versions and the
  published package revision participate in ordinary job release compatibility,
  invalidating cached dependency imports.
- The shared runtime reindex entry point invokes handler and command indexing on
  boot, activation/recovery publication, and observation of a shared source
  published revision, including commit switching. These boundaries pass only
  changed IDs after the first full node-startup pass; explicit `kernel.reindex`
  also supports a full rebuild.
- `DeclarationFiles` supplies shared flat TOML discovery for hooks, events, and
  `cbus/commands`. It treats filenames as opaque and validates real contained
  files and directories, rejecting nesting and symlinks. Only `.toml` files
  declare runtime behavior; other regular files, including `AGENTS.md`, are
  ignored. Documentation must never prevent package activation or indexing.
- Programs resolve from ready active database records and validate only the
  selected manifest and entrypoint. Invocation never runs Git, fingerprints or
  copies package trees, or acquires a repository lock. Entrypoints refer to the
  ordinary shared packages mount; activation owns publishing that source.
  Manifests may set `discoverable = false` without changing execution semantics.
  The optional boolean `uui` defaults to false and identifies interactive
  programs. Both flags are exposed in package inspection and ready-program
  metadata; neither changes generic invocation. UUI Home uses their conjunction,
  while explicit program selectors retain all ready programs. Package command
  candidates validate their referenced same-package program before source
  publication.
- Explicit program selectors use `ListPrograms`: ready package program manifests
  only, including non-discoverable runnable programs, bounded to 2,000 entries.
  They do not inspect Git status, services, or recursive files.
- Synchronization clones into a hidden same-filesystem staging directory,
  validates the exact commit, and replaces source by rename. Dirty shared
  worktrees are preserved and rejected. One failed batch package has explicit
  durable failure state.
- Activated-source consumers can require a clean installed checkout whose HEAD
  or content fingerprint exactly matches the ready database commit; schema
  evaluation uses this guard while staged activation evaluates its candidate.
- Git credentials never enter URLs, command arguments, durable Git config,
  package records, results, or logs. A selected named secret or recovery command
  secure input is used only for a host-scoped transient HTTPS authorization
  header.
- Repository pull is clean-branch fetch plus fast-forward; push publishes the
  attached branch; checkout selects one local/remote branch or hexadecimal
  commit. Changed operations use the same activation transaction.
- Kernel repository SQL remains portable across SQLite and PostgreSQL. Bind
  logical booleans as parameters or use boolean predicates; never encode them as
  SQLite-only `0`/`1` literals.

# Work Guidance

- Keep mutations and definition indexing package-targeted. Only published source
  updates scan current service Workers for observed imports; never precompute
  dependency graphs, reverse indexes, or asset dependencies.
- Keep filesystem ownership separate from database ownership: this package
  supplies candidates and commits; the database owner evaluates descriptors and
  applies physical schema.

# Verification

- Tests cover identity/path and program containment/symlink validation, ready
  metadata resolution without Git or filesystem artifacts, bounded inspection,
  database package CRUD and generic index revisions, clean Git synchronization
  and mutation, credentials, candidate-only schema activation, hook
  ordering/idempotence, durable phase recovery, PostgreSQL lock ordering,
  retirement/trim behavior, targeted package refresh, and automatic cross-node
  index propagation without application-table scans.
- Declaration regressions include documentation-only and documented TOML
  folders, candidate activation validation, and live handler reindexing.
- Deletion regressions cover dirty-source rejection, namespace containment,
  interrupted publication/recovery, removed programs/handlers, cross-node
  observation after source removal, recreation, and source-only registration.
- Hook regressions distinguish installed bootstrap sources without extra mounts
  from staged candidates requiring read-only overrides.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
