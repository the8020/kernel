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
- Package-index publication diagnostics are read-only annotations supplied by
  native command composition from the application indexer's pending owner; they
  are not persisted source state or source-readiness gates.
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
- `PackageRevisionFollower` observes the node's `packages:<nodeId>` revision and
  ready receipts. Bounded Git diffs yield resolved sandbox file paths, including
  deletions and both sides of renames. Each node has a separate checkout. Its
  Git objects must remain available for revision comparison.
- Startup registers clean checkouts at previously published commits. Unknown
  commits require sync before serving. A node may run an earlier accepted commit
  while another tests a new kernel. Node sync checks the reviewed HEAD and
  desired commit, stages from the selected transport, and publishes only on the
  recipient. Catch-up skips shared schema and hooks. Recovery belongs to the
  activating node. Retirement leaves other nodes serving until selected.
- The node-aware schema must be installed before starting nodes in this mode.
  Kernels started against the legacy schema use the system-wide active index
  until restarted. Align all serving nodes before the first mixed-version
  rollout.
- A removed package produces its sandbox directory with a trailing slash for
  observed-import matching. Removal does not require retaining its Git objects.
- On a source update, `ReactToSourceUpdate` requests an observed-import scan of
  current service Workers, deduplicates logical services, and requests a soft
  restart with that package revision as update identity. Import sets remain
  Worker-owned and jobs are never restarted or replayed. Unobserved late
  imports, stale caches, and failures in old work remain accepted shared-source
  tradeoffs.
- The database package record owns the desired and system-published versions
  after first initialization. Bootstrap TOML supplies fresh-database sources and
  explicit recommendations; ordinary boots register node receipts and never
  reapply recommendations or rescan all definitions. Release-staged repositories
  carry their resolved requested tag in local Git metadata; bootstrap verifies
  that it names the evaluated commit. Node-aware bootstrap pins the exact commit
  as both desired and installed. Legacy schemas retain the tag selector. At
  node-aware startup, a legacy tag that resolves locally to the active commit is
  pinned to that commit. Other legacy tags remain selected for explicit sync.
- A fresh database stages every bootstrap package and performs one batched
  evaluation and synchronization before publishing the package set. Normal
  activation evaluates only candidate package tables in one batch.
- Activation transactions use shared `act-` IDs. The initial database insert is
  exclusive, rejects collisions, and shares that identity across hook and
  activation history throughout recovery. A preparation retry must contain each
  recorded package and commit exactly once, in any order.
- Exact activation inspection reads the stage, candidate commits and hook
  checkpoints in one bounded database query, with errors limited to 2,048
  characters. It does not resume work or establish ownership, Worker liveness,
  schema settlement or safe lock release.
- `SynchronizeSet` accepts a fresh activation ID and 1..256 exact commit/tag
  selections with observed before commits, bounded to 256 KiB. Under sorted
  source locks and exact activation ownership it validates the whole baseline,
  rejects local/custom/named-secret configurations (including retired rows),
  dirty or unindexed sources, then fetches every candidate before one Prepare.
  It never applies recommendation skipping or splits the selection. All sources
  switch before post-hooks; per-source renames are not an atomic set snapshot.
- The packages-owned `synchronizations` table records non-secret desired and
  previous source settings with activation admission, bounded to the recovery
  reader's 512 KiB limit before staging. Desired settings and all active commits
  publish in the same transaction, only if previous settings still match.
  Fetch/handler validation publishes no records or source. Schema preparation
  can leave failed placeholders or forward-required receipts through the
  existing activation lifecycle. Unsupported schemas reject before staging;
  install the updated packages schema through existing synchronization first.
- Exact `ResumeSynchronization` accepts only activations recorded by that set
  owner, preserving development's separate journal and legacy recovery owners.
  It uses existing source/activation locks, verifies unchanged source settings,
  and resumes existing checkpoints; it never starts a new deployment. Completed
  hook phases remain skipped; a partly executed failed chain may repeat effects.
  Generic startup recovery applies the same settings guard to these activations.
- Recovery cannot append a missing dependency or replace a candidate in a
  retained activation. Uncommitted, unswitched work can settle through ordinary
  rollback and a fresh reviewed set. Committed schema requires the original
  candidates forward; dependency repair then needs an operator's schema-aware
  recovery plan. A non-overlapping install alone does not establish safety, and
  neither apply locks nor activation rows may be cleared to bypass this
  boundary.
- Installed activation is prepare schema → pre-activate hooks → switch source →
  post-activate hooks → publish package records/revision → index and clean up →
  complete. The `published` stage retains retryable indexing/cleanup without
  repeating schema or hooks. Pending work is bound to an explicit activation ID;
  independent package sets can progress while overlapping publications reject.
  Ordinary package tests exercise these same production sources.
- Native locks under `packages/.meta/activation-locks/` are created on demand by
  publication, retained for stable identity and acquired in sorted order.
  Repository operations, development activation and recovery share this owner.
  Release explicitly unlocks before closing so a concurrent child fork cannot
  prolong source ownership. Read-only observation never creates a lock and
  detects a first publisher appearing during its source read.
- Published commits and their revision come from one database snapshot.
  Preparing another activation never hides an already published package.
- An activation candidate with an empty commit removes its package; its root is
  the installed path. Removal executes no activation handlers from deleted code,
  retires the package record and tables, removes its active catalog commit, and
  publishes one revision through the same recovery/reindex path. New candidates
  acquire local package records automatically when no desired record exists.
- `DeletePackage` rejects uncommitted work and unsafe package/namespace paths,
  stages installed source in the existing `.previous` activation backup, and
  removes that backup after publication. Uninstalled desired packages, including
  empty mount placeholders, can also be deleted using the same safe placeholder
  cleanup as synchronization. Nonempty uninitialized/corrupt source still blocks
  deletion. Retained activation history and physical table data are preserved;
  reinstalling a retired package needs no manual index edit.
- Rollback restores previous ready package records before the schema evaluator
  rereads their still-active source.
- Removed table/column definitions become retired metadata and retained physical
  data. The db owner classifies safe changes and runs optional table migrations.
  Exact table.column force consent permits only incompatible-cell NULL/default
  repair after ordinary cleanup/native activation and confirmed rollback. Trim
  remains separate.
- Committed schema/catalog receipts prohibit source/schema rollback. Retain the
  candidate and phase; resume pre/post hooks, source switching and indexing
  forward without replaying committed schema work. Missing candidate source
  fails explicitly until that same candidate is restored. Ordinary activation
  preparation does not run a presentation preview or count rows for display.
  Hook checkpoints cover a package phase, not individual handlers: retrying a
  failed chain can repeat effects from handlers that already ran.
- Before retrying schema or hooks, recovery checks a retained Git staging tree
  is clean and matches its recorded candidate commit; a synchronization staging
  tree without Git metadata is rejected. If staging is missing, the installed
  fallback must itself match the candidate before it can be read.
- Before retrying switched-source hooks or schema completion, recovery checks
  each installed source is clean and matches its recorded candidate identity;
  removed candidates must remain absent. The same check precedes retrying
  published indexing and backup cleanup. A mismatch retains the activation and
  backup.
- First-install recovery treats an empty sandbox mount-target directory as an
  uninstalled package, using the same placeholder removal as synchronization. A
  nonempty unexpected directory remains an explicit source conflict and is never
  discarded. Resume the retained candidate without replaying committed schema
  work. If schema was not committed and the source switch is incomplete,
  recovery restores switched sources and aborts even when an unpublished
  candidate is missing; committed work still requires the exact candidate for
  forward recovery.
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
- Hooks accept `pre-activate`, `post-activate`, `index-packages`, or
  `index-services`. `PackageHooks` selects one package's phase and `Hooks`
  selects an ordered trigger chain across packages; both consume only the
  published memory index. Activation phases use a validated candidate index
  before live publication.
- `index-packages` extends the existing publication pass for application-owned
  indexes. Packages own declaration parsing, storage, and policy; the kernel
  supplies selected package/commit scope and ordinary hook execution only.
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
- Programs resolve from ready active database records. Only `entrypoint` is
  decoded from the selected program TOML, defaulting to `program.ts`; unknown
  fields are ignored. TOML syntax and the 1 MiB read limit remain enforced.
  Application manifest schema, descriptions, layout paths, and UI/discovery
  fields are not native models, validation rules, or response data.
- Invocation never runs Git, fingerprints/copies package trees, or acquires a
  repository lock. Real non-symlink entrypoints stay contained by the owning
  program and refer to the ordinary shared packages mount.
- Commands, hooks, and events share `ResolveProgramWithCandidates` for full
  `namespace/package/program` references: staged replacements take precedence
  over ready active packages, and deleted targets fail validation.
- Handler resolution errors identify the declaring TOML and referenced program;
  missing ready packages direct the operator to review a version or restore the
  package. This validates declared handlers, not all source imports.
- `ListPrograms` returns up to 2,000 ready executable identities with
  package/commit and entrypoint data. Selected package inspection uses the same
  executable validator, including diagnostics for invalid programs. Neither path
  reads application metadata; the packages application enriches its own catalog
  and selected inspection inside the Worker.
- Synchronization stages the existing Git repository, or clones on first
  install, validates the exact commit, and replaces source by rename. Preserve
  Git objects, branch names, and history when applying versions. Dirty shared
  worktrees are rejected except for explicit, non-preview DB-state sync. That
  command saves displaced work (including detached HEAD or an unpublished
  checkout branch) under the instance's private `checkout-recovery` directory
  before switching any selected package. A second ref at the displaced commit
  does not replace this backup of the original checkout. Inspect staged edits first, then check
  working files with a fresh staging index so assume-unchanged and skip-worktree
  flags cannot hide displaced contents. Each result's `recovery_copy` points to
  a complete local checkout. As the instance owner, copy it with
  `cp -a -- "$recovery_copy" "$new_private_path"` to a nonexistent scratch path,
  inspect `git status` and `git log --all`, and recover files or commits there.
  Do not reset a live source or replay hooks. Retire private backups only after
  verifying recovery. In ordinary batches without that explicit intent,
  following the upstream default branch only fast-forwards the installed HEAD
  and the branch selected for checkout, even if HEAD was detached. A clean ahead
  or diverged member rejects the whole selection during staging. A tag or commit
  may roll back only if named refs retain every displaced tip in the staged
  repository. A saved pin alone cannot discard unpublished work. Ordinary
  multi-package synchronization holds the selected source locks and stages every
  source before one Prepare for changed candidates. All changed roots switch
  before post-hooks and publish together; local peers must already be
  compatible. Node catch-up validates the published set, switches its checkouts,
  commits its receipts together and reindexes once without shared schema or
  hooks. Mixed sets use the activation coordinator, which skips effects for
  already-published peers. No-ops create no activation. The inherited
  source-lock bound is 256 selected IDs; never split into sequential
  activations. A batch failure marks every selected result unsuccessful. System
  activation retains partial switches and forward-required candidates under its
  exact ID; ordinary retry never resumes failed hooks. Receipt-only catch-up has
  no system activation ID. A failed rename restores switched checkouts before
  rolling back receipts. If restoration fails or the receipt commit is
  uncertain, retain staging and reconcile exact checkouts and ready receipts on
  that node before retrying.
- A requested commit ID, including an abbreviated ID, must match the resolved
  commit before schema preparation or source publication, including after an
  additional fetch. Tag selection still resolves through the selected tag.
- `InstallRecommendedPackages` validates the current kernel's bounded schema-1
  `scripts/bootstrap-packages.toml`, recovers unfinished activations overlapping
  its selection, then holds all selected source locks while staging candidates.
  Optional numeric `version` selects a tag; omission follows the remote default
  branch. Preserve local/custom sources, dirty work, local commits and newer
  versions before changing desired state. Fetch every candidate before one
  schema/handler/hook preparation against the complete set, including mutual
  imports. Declaration order controls reporting only. Unchanged/skipped peers
  remain installed and must be compatible; failures identify the invalid source
  or import and direct the operator to repair and explicitly retry.
- `RecommendedPackages` exposes that validated list;
  `InstallRecommendedPackagesFrom` accepts one application-selected transport or
  explicit skip for every entry. Resolve named credentials transiently with the
  selected Basic username, never into durable package settings. Rerouting
  preserves the configured upstream and its credential on legacy and node-aware
  schemas. Unrelated sources and local work remain protected. A changed Git
  checkout origin is published even when its commit is unchanged.
  Recommendations set the standard branch fetch refspec on the staged checkout
  and fetch tags from the selected source. Missing routes or credentials fail
  without upstream fallback; the store owns no system-role policy.
- A prepared kernel update binds the target manifest and selector routes to
  the active build. Before startup validation, selected nonlocal Git trees may
  be rebuilt from their previously published node commits; Git configuration
  and named refs survive. After startup, fetch the exact tag/default HEAD from
  the revalidated route, without changing existing origins or fetch refspecs.
  Reject redirected working trees and non-self-contained Git metadata before
  repair or staged checkout. Materialize the full target tree independently of
  client sparse-checkout rules. Source exclusions remain nonmutating. Publish the new candidates together;
  this authority does not extend to ordinary recommendations or synchronization.
- Fetch/validation failures before source switching publish no candidate roots
  or ready commits. Results distinguish `not_applied` candidates from unchanged
  or skipped packages when staging stops. Durable schema receipts and partial
  source switches retain every staged candidate and existing backup for forward
  recovery; no universal rollback is promised. Explicit repeated recommendations
  recover overlapping attempts before staging fresh work, without repeating
  successful schema/hook phases. Other pending activations remain untouched.
  Recovery verifies installed candidate identity even when `.previous` exists; a
  backup alone is not evidence that source switched.
- Activated-source consumers can require a clean installed checkout whose HEAD
  or content fingerprint exactly matches the ready database commit; schema
  evaluation uses this guard while staged activation evaluates its candidate.
- Package sources, bootstrap remotes, and Git credentials support HTTP and
  HTTPS. Transport security is the system owner's responsibility.
- Git credentials never enter URLs, command arguments, durable Git config,
  package records, results, or logs. A selected named secret or recovery command
  secure input is used only for a host-scoped transient HTTP authorization
  header. Synchronization accepts an optional Basic username with its secure
  password input; omitting it retains `x-access-token` for tokens. The same
  invocation credential is used if an exact commit requires a further fetch.
  Redact both the supplied secret and its actual encoded Basic header from
  failures.
- Staging an explicitly selected package record uses its named secret, not a
  separately stored package record's secret; invocation credentials take
  precedence. Staging itself does not authorize changing a protected source.
- Installed versions stay attached to the current branch, using the remote's
  default branch on first install. Tags and commits select the initial or
  applied version without detaching; branch tracking does not advance that
  version. Git owns branch state; the database continues to own the exact active
  commit.
- Direct Git operations on an indexed upstream package use the upstream saved in
  the database, including when the node still has a previous origin.
- Repository fetch updates remote references and tags without changing HEAD,
  worktree files, desired/active versions, or running services; dirty worktrees
  remain fetchable. Pull explicitly fast-forwards a clean attached branch; push
  publishes it without force. Commit checkout retains the branch.
  Source-changing operations use the same activation transaction.
- Read-only repository inspection holds the shared source observation lock and
  validates it before returning. `absent` means no package directory; `empty`
  means an empty uninstalled mount placeholder. Nonempty uninitialized source,
  invalid Git metadata and missing initial commits retain distinct diagnostic
  statuses; filesystem/permission errors are never absence. Git mutation guards
  still require an initialized repository.
- Successful synchronization preparation immediately registers abort cleanup,
  including failures reading the schema outcome before source publication.
  Receipt-aware completion retains forward recovery when required; cleanup
  failures are joined with the original error rather than discarded.
- After native activation recovery, startup validation permits desired/failed
  first-install records without an active commit only when native source
  inspection confirms absence or an empty placeholder. Active-source mismatch,
  unsettled activation, corrupt source and unindexed installed packages still
  prevent application readiness.
- A pending installation can inspect versions in a temporary bare repository.
  Opening Versions leaves its checkout absent.
- Bounded version listings include exact commit parent IDs. Local repositories
  select their current HEAD and never require or fetch an upstream reference.
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

- `TestOrdinarySynchronization*` covers complete-set schema and cyclic pre/post
  imports in both caller orders, no-op hook cardinality, missing/invalid peer
  refusal before publication, and failed post-hook publication without ordinary
  retry replay. The installed baseline fixture's `--batch-only` and
  `--batch-samples` modes exercise the public development command separately.
- Staged-set regressions cover both cyclic-import orders, exact recorded input,
  failed staging/validation, missing-handler provenance, source protection,
  unsupported schema/input and recovery without replaying successful phases. The
  package fixture executes real Git and Deno with a job double; deployments'
  native smoke separately exercises ordinary Workers through the actual caller.
- Tests cover identity/path and program containment/symlink validation, ready
  metadata resolution without Git or filesystem artifacts, bounded inspection,
  database package CRUD and generic index revisions, clean Git synchronization
  and mutation, credentials, candidate-only schema activation, hook
  ordering/idempotence, durable phase recovery, PostgreSQL lock ordering,
  retirement/trim behavior, targeted package refresh, and automatic cross-node
  index propagation without application-table scans.
- `TestRecoveryRetriesWholeFailedHookChain` uses a job double to verify that
  recovery resubmits the complete failed chain and skips a successful peer.
- `TestCommittedSchemaRecoveryFinishesPartialStagedSet` exercises real Git trees
  across a partial switch, rejects a missing unswitched candidate even when its
  commit exists in the old checkout, and resumes after restoration.
- Git regressions cover older-version installation with upstream tracking, fetch
  without activation, attached commit selection, retained local Git history, and
  fetch/select/push through ordinary repositories.
- Recommendation regressions cover missing packages, version/default-branch
  advancement, idempotence, downgrade/local-work protection, configuration
  validation before mutation, schema failures, all six three-package order
  permutations with real cyclic TypeScript imports, prepublication
  fetch/retained-peer failures, and durable post-hook recovery without repeated
  successful work.
- Declaration regressions include documentation-only and documented TOML
  folders, candidate activation validation, retained handler references that
  block deletion, and live handler reindexing.
- Deletion regressions cover absent/empty uninstalled source, nonempty/corrupt
  and dirty-source rejection, namespace containment, interrupted
  publication/recovery, removed programs/handlers, cross-node observation after
  source removal, recreation, and source-only registration.
- Hook regressions distinguish installed bootstrap sources without extra mounts
  from staged candidates requiring read-only overrides. A coordinator fixture
  evaluates cyclic imports through those mounts before source switching and
  through the installed roots afterward, in both candidate orders; it uses a job
  double, not sandbox Workers.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
