Parent DOX: [development DOX](../AGENTS.md).

# Purpose

- Investigate development workspace isolation, publication, durability, and
  terminal lifetime with reproducible, disposable experiments.

# Ownership

- `run.py prototype` builds separate opt-in kernel/admin/logd binaries under
  `.development/workflow-prototype` from the same sparse driver and activation
  owners used by the native fixtures. `prototype.go` supplies per-user driver
  connections and lifecycle integration; `sparse_activation_test.go` owns the
  preview and activation used by both fixtures and the compiled prototype. The
  build copies its custom `runsc` into that output directory and embeds that
  path; later experiment builds cannot replace the prototype's runtime. Set
  `WORKFLOW_PROTOTYPE_OUTPUT` to build a separate output while a review instance
  is using the existing binaries. Installed binaries and SDK caches remain
  untouched.
- [PROTOTYPE.md](PROTOTYPE.md) owns prototype build, operation, and native UUI
  verification instructions. Optimization and broader qualification are
  deferred.
- `start-prototype.py` starts the already built binaries in the separate
  `/tmp/8020-prototype-review` instance on HTTP 8082 and SSH 22222. First setup
  copies staged package sources, installs ordinary scripts and materializes
  prepared images once. Later starts preserve its database, sources and user
  work. It refuses an existing instance without its completion marker; it never
  rebuilds or upgrades an installed instance. Use native `/tmp` storage for
  runtime sockets; the workspace mount rejects their required chmod.
- `prototype-results.json` records compiled native browser/terminal handoff,
  package lifecycle, readable helper output and presentation verification. Its
  two-developer browser fixture publishes through each sandbox's real helper;
  untouched paths stay live during private editing and conflict resolution,
  including packages created after both sandboxes started. It does not qualify
  overlapping activation requests or multi-node source publication.
- `dev-core/programs/development-test/prototype_concurrency.ts` uses the built
  prototype in separate disposable nodes. A native pre-activation hook gates one
  package while a second developer activates another, checks overlap rejection,
  then retries into ordinary Git conflict resolution. Its
  `prototype-concurrency-results.json` covers that single-node SQLite path;
  multi-node/PostgreSQL publication and full workload contention remain separate
  gates. Never rebuild or replace the live manual review's binaries to run it.
- `development-shutdown-failure.json` records serial developer cleanup
  exhausting the kernel shutdown deadline. The prototype patch closes up to
  eight sandboxes concurrently, retaining each user's lock and
  checkpoint/stop/delete order. The owner regression pauses stops and checks
  private checkpoints before both sandboxes finish; larger shutdown populations
  remain unqualified.
- Ordinary package creation needs only a manifest and source files. Whole-root
  removal is captured and publishes an empty-commit candidate in the shared
  schema transaction. An upstream addition during removal becomes an ordinary
  added-by-them Git index conflict. Source switching and retry share the
  existing attempt journal; no separate deletion command is invoked.
- Native candidate mounts may leave an empty directory at an unpublished package
  destination. Treat only that empty directory as absent; Linux directory rename
  replaces it atomically and rejects nonempty destinations. Existing Git
  repositories must remain clean and match the recorded source head.

- Own the workflow analysis report, experimental harnesses, and recorded
  results.
- Own the Phase 1 xterm serializer qualification probe and raw broker benchmark
  observations; completed implementation status belongs to the parent's
  [implementation checklist](../WORKFLOW_IMPLEMENTATION.md).
- [REPORT.md](REPORT.md) owns recommendations; [RESULTS.md](RESULTS.md) owns
  reproduction, measurement boundaries, and verification observations.
- `current-results.json` retains the September 8 baseline refresh, including
  native Git metadata loss and inconsistent lower-file stat/hash observations.
- `phase2-results.json` retains authorized stock-runsc coherence and durable
  native Git experiments, publication/validation timings, and native transfer
  evidence. The explicit-sync alternative is rejected for workspace adoption;
  its component results do not satisfy freshness and small-edit storage costs.
- `upper-results.json` records the stock overlay with a durable gofer upper:
  asset-copy cost, runtime-loss persistence, retained-descriptor staleness, and
  the Git-triggered whiteout failure with directfs enabled and disabled.
- `gofer_probe.go`, `gofer_probe.py`, and `gofer-results.json` own the limited
  upstream-SDK Gofer transport experiment. It qualifies existing-file copy-up,
  originals, live reads, executable/mmap access, and completed-write persistence
  against 2 GiB of assets. The retained read record predates namespace support;
  source digests distinguish revisions. The complete workflow is unqualified.
- `gofer-write-results.json` records first and repeated existing-file saves with
  sync; `gofer-atomic-results.json` measures temporary-file replacement with
  file and parent-directory sync. `gofer-namespace-results.json` records mounted
  native Git commit/conflict/resolution and recreation, atomic saves, private
  links, locks, mmap, and local inotify through the same Gofer.
  `git-conflict-results.json` records native sparse conflict state, ordinary
  resolution, and retry after shared advancement without asset checkout.
- `gofer-rename-failure.json` retains the earlier shared-file rename failure.
  Files and package subdirectories now rename as per-file original/deletion and
  destination records. Shared content uses retained Git object references;
  existing private files move directly. The SDK excludes the affected subtree,
  and capture/acknowledgement cannot observe a partial batch. Ordinary metadata
  errors restore the private source and existing destination. Joint host-crash
  recovery remains outside this prototype qualification.
- `.references` in the private snapshots root checkpoints bounded filename and
  original maps, without payloads. Reads use the original shared inode while its
  version matches; the existing kernel-only Git connection materializes an
  individual retained blob when that backing changes. First content edits use
  ordinary copy-up. Capture records `file-reference`/`base-reference`; native
  Git preparation uses those object IDs and omits unedited moves from sparse
  checkout. Validation hardlinks matching blobs even when their names changed.
  These records and retained objects survive runtime recreation.
- `run.py rename` and `rename-results.json` verify native repeated/nested and
  empty-directory moves, mixed edits/creations/deletions, working-directory and
  open-file continuity, reference copy-up, metadata-error rollback, recreation,
  Git marker and modify/delete conflicts, resolution, activation, later edits
  and zero private asset copies. Git may report a completely rewritten file as
  modify/delete at its original path; resolution uses ordinary Git. Timings are
  single observations with a checking hook, excluding the actual schema engine.
  Metadata-reference renames require the native Git owner in `run.py` profiles;
  standalone Gofer transport profiles do not supply that owner.
- Shared symlink deletion/rename and link-target conflicts use the same native
  original/capture/Git path. Captures retain symlinks and Git mode `120000`;
  comparisons and publication never dereference targets. The activation fixture
  verifies side selection through the UUI program's sandbox helper. The extended
  standalone namespace fixture still fails retained symlink descriptors after
  unlink; `symlink-handle-failure.txt` retains the earlier replacement failure.
  The SDK readlink overlay removes its deleted-node rejection and propagates
  errors, but does not qualify that remaining unlink case.
- `gofer-nested-deletion-failure.json` records child deletion metadata hiding
  its parent and siblings. Only deletion marker files hide paths; directories in
  that metadata tree are structural. Native checks and helper activation
  preserve nested originals, live siblings and later shared recreation.
  Private-only directory removal uses native `rmdir`, including nonempty
  rejection.
- `gofer-shared-directory-failure.json` records rejected shared-directory
  removal. Native removal/recreation now preserves child originals, rejects
  nonempty directories, and hides later shared additions across recreation.
  Synced `.directories/<path-sha256>` records in the private snapshots root
  contain removed directory paths; they hide lower descendants while allowing
  new private children. The `changes` response reports these paths separately.
  Activation captures a hardlink to each marker and acknowledges directories
  after their files, deepest first. Native mkdir/rmdir refresh marker identity;
  separate directory heads retain later operations and reject stale attempts.
  Small linked receipts prevent inode reuse while captures remain referenced.
  Held directory references keep their marker. Whole-package lifecycle uses the
  same capture and publication attempt; marker/upper changes still need joint
  host-crash qualification.
- `gofer-removed-directory-failure.json` records the SDK rejecting enumeration
  of a removed directory with `EINVAL`. A one-line `Getdents64Handler` overlay
  returns no entries for that removed node, matching native fixture behavior and
  preventing lookup through its reused path. `gofer_probe.py` owns common SDK
  overlay preparation for standalone and real-driver fixtures; both record the
  source/overlay hashes and leave cached/installed runtimes untouched.
- `gofer-publication-results.json` records filesystem-owned capture and
  acknowledgement, later-write preservation, quiescent-file retirement,
  completed-checkpoint recreation, and the small-read cost of disabling the
  package dentry cache. It does not qualify the activation helper or recovery
  from an interrupted publication. Earlier read/write records identify earlier
  source revisions and cache settings.
- `gofer-directory-read-before.json` preserves the earlier 31-sample small-read
  comparison. The targeted repeat after directory tombstones measures their
  lookup cost without repeating broad tree benchmarks; keep the simple lookup
  while the prototype's small working-set cost remains acceptable.
- `sparse_test.go` and `sparse-runtime-results.json` integrate the sparse mount
  with the real development driver, retained PTY broker, authenticated helper,
  and command bus. Native conflict worktrees and later edits survive driver
  recreation. The fixture gives Git private metadata with read-only retained
  object alternates and no source checkout. It does not replace activation's
  publication owner or qualify schema/hooks or all packages.
- `sparse-object-retention-failure.json` records shared GC breaking private
  history. First Git access and selected-package preparation hardlink shared
  objects into private `borrowed/` storage with source-read validation or the
  publishing package's lock. Its permanent read-only mount remains available
  after shared removal; private Git can never write those inodes. Native checks
  cover loose/packed links, denied write/alias attempts, GC, shared removal,
  unresolved conflict recovery, and replacement-repository fetch without
  changing the private branch. The ordinary remote uses one read-only
  shared-root mount. Retention requires hardlinks and independently complete
  shared repositories; shared alternates and promisor objects reject
  initialization. Walks stop at 100,000 entries per package. Links currently
  last for the workspace lifetime; repeated repacks, reclamation, and host-crash
  durability remain unqualified.
- Package `.git` entries are standard reference files to private metadata under
  `/workspace/git/private/`, mounted outside the source tree. The native rename
  failure in `sparse-gitdir-mount-failure.txt` rejects the former per-package
  submounts. Reference rename/removal and recreation preserve private history
  and conflict worktrees. Initialization confines writes to the upper and
  preserves existing or deleted references. Activation excludes Git metadata;
  lower-alias discovery prunes directories hidden by private files. Native
  activation checks cover whole-package removal and private edits against an
  upstream package deletion.
- Before exposing a later-published package's `.git`, the Gofer requests private
  initialization through its kernel-only Unix connection. The existing Git owner
  retains objects with source-read validation and builds metadata outside
  developer-visible mounts, then installs it through a confined directory handle
  without replacing existing state. `prototype-peer-git-results.json` records
  the earlier shared-metadata dependency and the native commit/deletion/restart
  regression. Ordinary Git commands require no wrapper or package checkout.
- `sparse_activation_test.go` and `sparse-activation-results.json` exercise the
  real helper/HTTP/command-bus path with a disposable replacement of
  `Manager.Activate`. Captured per-path originals feed native sparse Git
  conflict worktrees; a durable attempt retains resolution across retries. The
  checking schema hook verifies validation-before-source, later edits, rejection
  and shared-advance retries. It is not the actual schema engine or production
  activation coordinator. Deletion, lower hardlinks, and retained PTYs have
  focused checks. Preparation intent is saved before calling the shared owner;
  retry aborts that exact preparation before starting a fresh transaction for
  the retained native candidate. After publication and acknowledgement, an
  explicit filesystem-owner `release` removes captured `file`/`base` payloads.
  Synced identity receipts remain for retries and ID reservation; live originals
  stay in `base/` and native Git retains the merge history. Pending captures
  cannot be released. Cleanup failure retains the published attempt for retry.
  Directory checks resolve native Git modify/delete conflicts with ordinary
  `git rm`/`commit`, preserve new shared children, and restore live shared
  paths. A later remove/recreate cycle survives a lost reply and runtime
  recreation; a fresh directory-only activation uses an empty sparse worktree.
  Ignore filtering preserves paths tracked by either the private index or
  current shared tree. Full qualification remains open.
- `activation-error-failure.txt` records a failed helper response losing its
  reason before any package result exists. The shared activation result now owns
  `error`; disposable overlays retain it in saved sandbox state and include
  gateway errors in that same HTTP result. Native checks cover preflight errors
  and conflicts through the helper. Human CLI output and the dev-core UUI editor
  share native Git worktrees through the platform's bounded Git adapter; native
  checks also reject stale UI saves after terminal edits.
- `sparse_schema_test.go` and `sparse-schema-results.json` join that activation
  owner to the ordinary native job/Worker runtime, Deno table evaluator, SQLite
  schema engine, and package activation coordinator. They check incompatible
  schema rejection, a native Git candidate fix, actual pre/post hooks, retained
  data, and retry with recreated coordinators after database completion but
  before private-file acknowledgement. Registration/heartbeat/database-scope
  callbacks are fixture acknowledgements. Recovery checks interrupt preparation
  and a two-package source switch at clean Git boundaries, then recreate the
  coordinators. Dirty shared trees must remain untouched; later private edits
  and the development runtime must survive. Mid-reset dirty trees, PostgreSQL
  concurrency, and host power loss remain unqualified.
- `activation_cost_test.go` and `activation-cost-results.json` compare five
  single-label edits/activations with 4 MiB and 2 GiB of tracked random assets.
  They use the native helper and a checking validation/completion hook, without
  the actual schema engine. Record retained private storage, shared validation
  inodes, process I/O/CPU counters and sampled RSS; fixture setup is excluded
  and host caches are not flushed. `activation-cost-before-release.json`
  preserves the earlier run before captured-payload cleanup. Current storage
  accounting rechecks a saved shared inode against its live source path before
  excluding it; inode-number reuse must not hide new private files. The older
  counter could undercount recycled inodes, so use current storage totals.
  `activation-cost-before-object-retention.json` preserves the last run before
  borrowed-object links. Current accounting recognizes unchanged shared Git
  inodes across all staged packages, including guidance.
- `activation-link-read-results.json` isolates a cost of the validation view:
  creating and removing hardlinks changes ctime and makes native Git rescan
  unchanged asset contents. Cheap retained storage alone does not qualify this
  publication path's I/O cost.
- `activation-transaction.patch` is the disposable shared-owner change binding
  schema/package preparation and completion to one explicit `act-` identity. It
  updates all current shared-handshake callers and stores that identity in the
  pending schema record. `transaction_test.go`, `transaction-failure.json` and
  `transaction-results.json` retain the failing unbound completion case and the
  repaired regression. A late completion must never consume a different prepared
  activation. `transaction-rollback-failure.json` records the failed rollback
  incorrectly marked terminal; incomplete cleanup now remains pending and
  retryable. `preparation-recovery-failure.json` records missing preparation
  intent and rejection of aborting an ID whose preparation never started.
  Aborting an absent ID is harmless; successful completion still requires a
  known ID. Callers must never reuse an abandoned ID.
- `concurrent-catalog-failure.json` records unrelated package deployments
  blocked by the database's singleton pending record. The patch allows distinct
  package sets, rejects overlap, looks up completion by exact ID, and applies
  only that deployment's package changes to the latest catalog. A bounded scan
  admits at most 256 pending deployments with 256 packages each. Interleaved
  completion, removal/rollback isolation, and pending-status checks pass at this
  owner. These catalog checks alone do not qualify concurrent activation.
- `concurrent-coordinator-failure.json` records an unrelated activation waiting
  behind another package's pre hook. The patch removes the coordinator's single
  current attempt and the evaluator's retained deployment state. Each executing
  prepare/complete/recovery call acquires exact-ID operation ownership;
  duplicate requests fail promptly. Durable unfinished package rows reject
  overlapping candidates between calls. A short metadata lock admits at most 256
  unfinished activations through the existing stage index; evaluation and hooks
  run outside it. Initial bootstrap/full synchronization still use exclusive
  deployment ownership. SQLite checks pause a hook or evaluator and complete
  another package, then verify overlap rejection, recreated completion and
  rollback isolation. `transaction-race-results.json` retains the focused race
  checks. PostgreSQL advisory ownership is implemented in the patch but
  unqualified.
- `source-ownership-failure.json` records recovery rolling back a checkout
  between preparation and source switching. The patch extends the package
  owner's existing lock to native shared `packages/.meta/activation-locks/`
  files. Activation, synchronization, repository mutation, deletion and recovery
  use that same sorted, nonblocking ownership through preparation, switching and
  completion. Never unlink lock files when packages move or disappear. Readers
  and private workspace edits do not acquire them. Separate processes verify
  exclusion, unrelated progress, partial-acquisition cleanup, stable ownership
  across directory replacement, and release on process death. Startup handling
  of live pending activations, runtime indexing and PostgreSQL/shared-storage
  deployment qualification remain open.
- `publication-index-failure.json` records completion becoming terminal before
  runtime indexing succeeds. The shared patch now atomically publishes package
  records/revision with a `published` activation stage; only successful indexing
  and source-backup cleanup mark it complete. Retry/recovery resumes that stage
  without repeating schema work, hooks or revision publication. Published
  attempts retain their package claims and reject rollback. Ordinary, bootstrap
  and recovery paths share finalization; checks also reject a failed
  completion-record write. `activation-stage.patch` adds the matching enum value
  only to the copied packages table definition used by the native schema test.
  Full runtime convergence and startup handling remain separate qualification
  gates.
- `batch-recovery-failure.json` records one busy attempt preventing recovery of
  unrelated abandoned attempts. Its negative control retains the current safety
  checks and substitutes single-attempt dispatch. Recovery now snapshots at most
  256 unfinished IDs, closes the database cursor, and visits each with its own
  source/operation ownership. Errors remain visible after unrelated recovery
  proceeds. Startup checks activation records even after schema completion,
  inspects source after recovery, and rebuilds its initial index from the
  settled package set. Native schema checks retain one live preparation, roll
  back two abandoned ones, then recover the released owner. Starting a node
  during live publication remains unqualified; startup continues to report busy
  recovery as failure.
- `published-snapshot-failure.json` records a follower pairing stale filesystem
  commits with a newer revision and treating a preparing package as removed. The
  shared package index now reads published commits and their revision in one SQL
  statement, retaining an active commit during preparation or failure until
  publication or retirement changes it. Follower initialization uses that
  snapshot immediately before the full startup index; unchanged polls remain a
  scalar query. The native schema fixture observes bootstrap and an earlier
  publication while the same package's next activation prepares.
- `preparation-availability-failure.json` records preparation hiding published
  packages/programs, dropping selectors and breaking other packages' handlers
  and activation hooks. The shared patch removes that package-wide disable step.
  Existing ready records retain their published identity; activation rows own
  unfinished stages and errors. First activation remains unavailable until
  publication. Shared source is still mutable, and exact-source schema checks
  remain in place. The native fixture invokes an ordinary program job while its
  package's next activation is prepared.
- `runtime-indexing-failure.json` records an unrelated refresh waiting behind a
  provider job, and a new request unable to enter during older work. Runtime
  indexing now claims only its selected package IDs under a short memory lock;
  database reads, provider jobs and runtime reconciliation run outside it. A
  request arriving during the same package's job coalesces into a retained
  refresh. Retry skips running owners; an older success cannot clear new work.
  Changed provider plans retain their requests for retry.
- `synchronous-reindex-failure.json` records explicit updates failing when they
  overlap background indexing. Synchronous requests publish available packages,
  release their claims, then wait for selected busy owners and refresh current
  state. Completion channels belong to those owners; no mutex or other package
  claim spans the wait. Cancellation leaves unfinished work pending. Background
  refreshes remain coalesced and bounded.
- `revision-consumer-lock-failure.json` records the outer revision consumer
  retaining the same long lock after the indexer was repaired.
  `revision-acknowledgement-failure.json` records an older completion failing
  after a newer revision was acknowledged. Revision calls now run independently;
  topology scheduling and diagnostic state use short locks. Both followers
  ignore older/duplicate completions without consuming a newer pending snapshot
  and still reject zero/future acknowledgements. App checks use an actual SQLite
  index follower and a paused provider.
- `revision-monitor-failure.json` records periodic polling waiting behind a
  provider despite those request-level fixes. The monitor now queues service
  work through the same indexer and per-package pending set. At most 16
  background batches run; overflow remains pending. Each batch retains the
  ordinary five-minute bound, and shutdown cancels and joins them. Command and
  activation entry points retain synchronous indexing. The actual monitor loop
  with manual timer ticks and SQLite revisions verifies unrelated progress,
  health gating/recovery during a paused provider, and no duplicate running
  provider. Owner checks cover overflow retry and shutdown. Providers and health
  probes in these checks are doubles.
- `declaration-lock-failure.json` records handler and command refreshes waiting
  behind another inspection. `declaration-order-failure.json` shows that
  removing only those locks permits stale event/program references and command
  fragments to overwrite newer publication. The copied owners now inspect
  outside their memory locks, compare an in-memory publication counter, and
  rebuild from the current snapshot if another refresh published. Both handler
  kinds still publish together; command collision checks cover the complete
  cached catalog. Checks cover cross-package program references, unrelated and
  same-package commands, and a concurrent full rebuild. Handler metadata uses
  SQLite; command metadata uses the existing test source. Sustained contention
  can repeat selected inspection until the caller's deadline. Package-path Git
  comparison still retains its follower lock; full concurrency remains open.
- `sparse-storage-failure.json` and `sparse-git-io-failure.json` retain earlier
  persistent and temporary asset-object copying. `sparse-directory-failure.txt`
  records parent identity/submount loss; `sparse-alternates-failure.txt` records
  cold-index object copies despite alternates. `sparse-setstat-failure.txt` is
  the unpatched Sentry's false-success negative control.
- `sparse-new-deletion-failure.txt` records loss of a later deletion when a
  captured new file had no lower counterpart. Deletion intent must survive
  independently of whether shared storage already contains that name. Capture
  registers the path in `snapshots/.pending/` before opening its source; failed
  recapture restores the prior registration, and acknowledgement clears its
  matching entry. Ordinary uncaptured temporary files leave no deletion markers.
- Production development behavior remains owned by the parent.

# Local Contracts

- Keep prototype changes in source and verify them in disposable fixtures.
  Update a manual test deployment only when the user explicitly requests it.
- Group sandbox Git storage under `/workspace/git/`: writable `private/`,
  read-only retained objects in `borrowed/`, and read-only shared repositories
  in `shared/`. Keep package-root bookkeeping under `packages/.meta/`, with
  native source locks in `activation-locks/`. Sandbox startup never initializes
  every package's Git metadata. Ordinary Git access initializes only its
  selected package and creates no lock files. Activation/source publication
  creates locks on demand; their identities remain for the instance lifetime.
  Reads use an existing shared lock or validate that no first publisher appeared
  before exposing derived state; only the OS lock is temporary.
- Selected-file preview compares saved per-path originals with current private
  contents, including retained Git references for directory/file moves. The
  Gofer's changed-path response includes only the relevant reference metadata;
  preview never creates activation captures or acknowledges edits. Load at most
  48 KiB per text side on demand and show a notice for larger, binary, or linked
  files. Native Git computes the hunks; dev-core owns all rendering.
- Experiments use temporary repositories and sandbox roots. Never target a
  developer's existing sandbox or publish into source-workspace repositories.
- The shared native fixture stages the actual sibling `dev-skills` package and
  installs ordinary scripts through `installTestDevelopmentAssets`; default
  mount requirements remain in force. `analysisStagePackage` shares package
  source staging with the schema fixture and excludes Git metadata, ignored
  files and environment files. Native records retain the guidance Git tree.
  Never stage empty stand-ins for required guidance.
- The schema fixture records original package-input hashes, the stage-definition
  patch digest, and Git trees of the final staged packages before bootstrap.
- Characterization of a defect is evidence, not an accepted behavior contract.
- Keep prototypes separate from production and distinguish measured results,
  proposed behavior, and remaining validation gates.
- Evaluate asset-heavy packages as well as small-file counts. Record initial and
  recurring bytes copied, allocated storage, freshness of unrelated files in the
  same package, and conflict behavior while the developer keeps editing. A
  successful component benchmark cannot close unmet workflow requirements.
- The measured read overhead is accepted for the prototype. Prioritize writes
  and correct activation/merge behavior; do not repeat broad read benchmarks
  without a concrete new concern. Conflict handling must expose ordinary Git
  markers/files or unmerged index stages with native resolution commands.
- Finish the working prototype, including package deletions and the shared
  CLI/UUI conflict editor, before further optimization or broader filesystem
  qualification. Use focused end-to-end checks for that delivery.
- Directory renames reuse per-file overlay change tracking and preserve existing
  private edits. Renaming an asset directory must not copy its unedited payloads
  into private storage. Keep verification focused on this operation and its
  existing activation/conflict path; broader qualification remains deferred.
- Package and namespace renames also retain their original/current package IDs
  in `.references`. Activation includes both ends of a move and all packages of
  a removed namespace in one transaction, then acknowledges the captured
  namespace and rebases any later private moves. Discovery reads the merged
  manifest, including retained blob references, and never resurrects removed
  package roots while initializing Git.
- Plain new package folders need a regular `package.toml`; activation
  initializes missing Git metadata. Existing private Git history is retained,
  and managed `.git` redirects are rebound through the filesystem owner. Managed
  metadata leaves `core.worktree` unset so ordinary Git follows a moved
  redirect's parent. Cross-package captures add the required retained object
  stores to native Git alternates. New shared repositories remain independent;
  package renames reuse existing source and Git-object inodes instead of copying
  unchanged assets. Private metadata cloning runs in the sandbox; its filesystem
  owner links immutable private objects through confined directory handles.
  Object/index preparation does not require the removed package directory to
  exist.
- Hand over the runnable prototype once those checks pass. Further qualification
  stays recorded as unfinished and must not delay manual use.
- Do not make exceptional symlink/descriptor compatibility a prototype delivery
  gate without a demonstrated package need. Keep known failures explicit for
  later qualification; do not introduce a package requirement to use links.

# Work Guidance

- Run Git against developer-controlled metadata inside the sandbox. The native
  transfer probe uses ordinary Git bundles over the existing stream boundary;
  host Git benchmarks operate only on trusted disposable fixtures.
- Measure the pinned runtime and record repository shape, environment, command,
  sample count, and the boundaries included in each timing.
- Count Git objects and temporary write I/O as well as private source paths;
  zero copied `assets/` entries alone does not establish cheap small edits.
- The activation candidate links unchanged regular files for its native
  validation view, then writes only changed blobs. Lower-file copy-up preserves
  visible hardlink aliases and excludes hidden lower directories, nested mounts,
  and host-only aliases. Its metadata walk refuses more than 100,000 lower
  entries; this cost and installation races remain qualification gates.
  Changed-path enumeration is private-only and refuses more than 4,096 paths or
  a one-MiB encoded list.

# Verification

- `python3 kernel/development/analysis/start-prototype.py` exercises the manual
  launch path using already built artifacts. Check login readiness and ordinary
  administration; use the existing native browser record for merge/deletion
  coverage instead of rerunning it after documentation or launcher changes.
- Run from the kernel repository root with its installed local Go toolchain,
  pinned runsc, and development image.
- `python3 kernel/development/analysis/run.py runtime` exercises real sandbox
  activation, data loss, helper results, PTY cleanup, and tmux/WebSocket
  lifetime.
- `python3 kernel/development/analysis/run.py races` characterizes activation's
  capture/pause and repository-lock boundaries with deterministic fixtures.
- `python3 kernel/development/analysis/run.py git` checks native private
  commits, checkpoint/restart, and Git inspection after untouched shared files
  change. It reports failures of the existing backend; it is not candidate
  qualification.
- `python3 kernel/development/analysis/run.py fuse` runs the minimal external
  FUSE experiment; `WORKFLOW_PROBE_FILES=10000` selects the larger tree.
- `python3 kernel/development/analysis/run.py coherence current` fails on the
  current mount's incoherent lower metadata; `coherence shared` tests a
  temporary corrected default-profile runsc configuration. A pass covers
  coherence, not durable source or general mount-profile support.
- `python3 kernel/development/analysis/run.py native` tests disposable
  independent private Git repositories: native syscalls, conflicts/retry,
  retained PTYs, runtime-loss durability, and transfer. It builds
  `native_probe.go` as a temporary workload. Publication bypasses the production
  schema coordinator, and untouched files deliberately require explicit sync;
  this is not a complete release gate.
- `python3 kernel/development/analysis/run.py upper` tests a durable native
  upper against 64 MiB of allocated random assets. It grants mount authority
  only inside the disposable sandbox. `WORKFLOW_UPPER_DIRECTFS=false` selects
  the stock gofer RPC path; both paths currently fail freshness with a retained
  descriptor and panic on Git's index replacement when host whiteout creation
  returns EPERM. A failing run records rejection, not successful qualification.
- `python3 kernel/development/analysis/run.py sparse` builds the custom Gofer
  into the real driver using disposable Go source overlays. It checks parent
  identity/submount stability, unsupported-metadata errors without copying,
  helper preview write costs, native diff3/index stages, ordinary resolution and
  retry after shared advancement, incremental bundles, retained PTYs, and
  unresolved/resolved Git state across runtime recreation. Four allocated 1 MiB
  assets are tracked in Git. Preview timings include helper/runtime exec; they
  are not completed-activation benchmarks. It now applies the shared transaction
  patch too, so initialization uses the actual package source-lock owner.
  Retention checks delete all shared refs, prune unreachable objects, remove the
  repository and recreate the runtime; ordinary Git still reads assets and
  resolves retained conflict stages. Replacing the shared repository preserves
  private history and permits native fetch through the permanent remote mount.
- This sparse profile also overlays two owning fixes: Sentry shared-mount
  `setStat` must return its failure, and activation scan index initialization
  refreshes clean stat entries before staging. Installed runtime and production
  source stay unchanged. A hardlinked local SDK input tree permits Go compiler
  overlays without modifying the module cache. `run.py sparse unpatched` omits
  only the Sentry fix and must fail the direct `utimensat` error check.
  Standalone `gofer_probe.py` profiles omit the Sentry fix; their namespace
  passes do not qualify that error boundary. Both builders now overlay the
  separate removed-directory enumeration fix. The `unpatched` sparse profile
  omits only the Sentry fix.
- `python3 kernel/development/analysis/run.py activation` additionally overlays
  the candidate activation owner. It verifies on-demand text diffs for private
  edits, additions, deletions, and retained rename references after upstream
  advances. Use the ordinary helper and native Git to conflict, resolve,
  recreate, retry and publish while preserving later edits and a retained PTY.
  The hook receives a complete native candidate tree whose unchanged assets
  share source inodes. Check deletion/recreation and later deletion of captured
  new files, schema rejection, and source advancement during validation. Each
  helper timing is one observation including runtime, transport, Git, and the
  checking hook; it excludes real schema/hook execution. Source/SDK digests
  identify the tested revision. The shared package owner now supplies stable
  native locks through preparation and completion too. Recovery checks every
  selected HEAD and clean worktree before any mutation. All-switched attempts
  complete; partial clean switches restore previous heads and roll back the
  exact transaction before retry. Dirty or unexpected shared trees fail closed,
  including ambiguous interruptions within native reset. Release checks retain
  pending conflict bytes, remove acknowledged payloads, preserve identity
  reservations and next originals, and repeat acknowledgement and release after
  runtime recreation. An injected lost release reply leaves the attempt
  published; recreated retry finishes it without another prepare or loss of
  later edits. Directory checks also preserve later removal generations, reject
  stale acknowledgements, and publish upstream-tracked files despite an older
  private index and matching ignore rules. Private Git initialization also
  rejects a developer-controlled path escaping its mount before installing valid
  metadata. Small receipts and Git worktree/index cleanup remain separate
  lifetime concerns.
- `run.py rename` also checks package/namespace preview and activation, coupled
  package selection, retained private history, unchanged asset inodes, ordinary
  package creation with and without Git, cross-package file moves, namespace
  removal conflicts resolved through ordinary Git, and actionable
  missing-manifest errors through the same preview owner compiled into the
  prototype. The schema profile also checks retirement of the old catalog entry,
  registration and program execution under the new ID, and retention of the
  published Git history.
- `python3 kernel/development/analysis/run.py cost` measures five sequential
  label edits and complete helper activations against four and 2,048 tracked
  one-MiB random assets. The checking hook verifies shared asset inodes and
  publication, and the fixture rejects private asset copies or asset-sized
  retained data. Timings include command startup and checking-hook inspection;
  actual schema/package hooks, setup and post-activation assertions are outside
  them. Report the first sample and repeated samples, cache conditions, storage
  allocation and resource-counter boundaries. Do not run native modes together.
- `python3 kernel/development/analysis/git_probe.py hardlinks` uses Git Trace2
  to verify zero content scans before links, four after link creation, zero
  after index refresh, and four after link removal. Files remain unchanged and
  clean; the check separates ctime changes across seconds for Git builds with
  coarse stat comparisons. It characterizes the I/O defect without weakening
  dirty-source detection.
- `python3 kernel/development/analysis/run.py schema` adds the actual database
  and activation owners. It stages tracked package working sources without
  `.git` or environment files and uses the already materialized
  `.development/named-terminal-test/node/kernel/runtime/images/rootless` image
  read-only. Raw results retain image and staged-source digests. Bounded native
  runtime logs accompany failure. Timings are single helper observations; the
  injected completion error occurs after real hooks/database publication, and
  retry recreates coordination objects, not the kernel or host process. Another
  actual transaction prepares before that retry; its package/schema records
  remain pending until its own explicit rollback. Further cases interrupt before
  and after preparation, and before and after the last package's native reset.
  Assert exact prior transaction outcomes, hook counts, matching source/catalog,
  preserved later edits, and refusal to overwrite external shared changes. An
  injected handler-refresh failure leaves source/schema published and the
  activation unfinished. A recreated owner retries through the real helper,
  publishes the handler index once successfully, and preserves a subsequent
  private edit without repeating activation hooks or the package revision. A
  three-attempt recovery check holds one source owner while two other schema
  preparations roll back through the actual evaluator. Releasing that owner
  permits recreated recovery to clear the remaining exact schema/activation ID.
- `python3 kernel/development/analysis/run.py transaction` applies the shared
  contract patch to copied compiler inputs, runs the existing package, database,
  evaluator, development and deployment checks, then tests late completion,
  unknown successful completions, harmless unstarted aborts, terminal-outcome
  mismatch, identity reuse, retry after failed rollback, and independent catalog
  deployments with overlap rejection and removal/rollback isolation. It also
  pauses one package's hook while another completes; evaluator owner tests do
  the same during table evaluation. Exact operation ownership, bounded
  stage-indexed admission and recreated completion use the shared database
  owner. A paused native checkout verifies that recovery cannot take over its
  prepared source switch while an unrelated checkout completes. Child processes
  check stable native package ownership and release after process death.
  Publication checks cover failed index refresh and final-record writes,
  recreated completion/recovery, overlap refusal and unchanged revisions/hooks.
  Batch recovery checks that a busy publisher leaves unrelated abandoned
  attempts recoverable. Published-snapshot checks preserve active identity
  during preparation and the cheap unchanged polling path.
  Published-availability checks cover programs, selectors, source verification
  and other packages' handlers/hooks. Application, command discovery and
  program-runner checks cover the copied composition; live multi-node boot
  remains unqualified. Runtime-index checks preserve unrelated progress, later
  refresh requests and concurrent revision acknowledgements. Monitor checks
  cover progress during provider execution, bounded background admission,
  retained overflow and cancellation/join at shutdown. Declaration checks pause
  source inspection and verify independent publication plus rejection of stale
  snapshots. Synchronous reindex checks pause a background provider, change its
  configuration, cancel a waiting caller, and apply the current state without
  blocking unrelated packages, including a partially overlapping revision batch.
  The ordinary owner pass now includes app, command discovery and program-runner
  tests; `GOFLAGS=-race` enables their race checks too. The `activation` and
  `schema` modes use the same patch and persist the transaction ID in their
  native attempt. Production files remain unchanged. The negative record
  contains the previous regression source and original input digests.
- `python3 kernel/development/analysis/git_probe.py` runs disposable Git merge,
  transfer, ref-publication cases, and candidate preparation benchmarks.
- `git_probe.py native` measures clone/capture/merge/source publication with one
  reused validation worktree; `git_probe.py native-fresh` compares fresh
  validation checkout/removal. Both exclude schema/hook execution and production
  coordination.
- `python3 kernel/development/analysis/git_probe.py conflict` checks a native
  sparse Git conflict worktree: diff3 markers, all three index stages, ordinary
  `git add`/`commit`, retry after another shared update, and zero materialized
  assets. It is a trusted host Git fixture, not integrated activation.
- `python3 kernel/development/analysis/gofer_probe.py` builds a disposable
  custom runsc from the pinned generated SDK. It uses a private user/mount
  namespace, a read-only bind of shared fixture sources, and native private
  upper/original directories. The default fixture has 2,048 allocated random 1
  MiB assets plus 10,000 small source files. `WORKFLOW_GOFER_ASSETS=4` reduces
  only the asset fixture for diagnosis. The script records component checks and
  sequential sparse/stock-RPC/stock-directfs traversal samples, with debug off
  during timing. A successful exit does not qualify complete namespace and
  metadata semantics, transaction recovery, private Git ownership, activation,
  publication, or the full filesystem.
- `python3 kernel/development/analysis/gofer_probe.py writes` uses four small
  asset fixtures and benchmarks 31 first/repeated saves at 64 bytes, 64 KiB, and
  1 MiB through sparse, stock RPC, and directfs mounts. Timing is inside the
  sandbox and includes open/truncate/write/file-sync/close. It checks every
  resulting file and the sparse profile's originals. `atomic` measures the
  corresponding temporary-file save, rename, and parent-directory sync. Both
  modes run the functional namespace checks and leave the read record intact.
- `python3 kernel/development/analysis/gofer_probe.py namespace` runs functional
  checks without traversal or write benchmarks. It reuses `native_probe.go` for
  native file operations, including shared regular-file renames and retained
  descriptors, nested deletion, and private/shared directory removal/recreation,
  and runs Git inside the mounted workspace. A shared commit causes diff3
  markers and three unmerged index stages; those survive runtime recreation and
  resolve through ordinary `git add`/`commit`. This single-text-path case does
  not qualify activation or protect concurrent edits during conflict
  installation. Storage totals deduplicate hardlinked regular inodes and count
  symlinks/deletion markers separately, excluding directory allocation.
- `python3 kernel/development/analysis/gofer_probe.py publication` adds a
  private fixture control socket to the Gofer and uses trusted host Git to
  publish captured files. It checks clean retirement, later atomic/descriptor/
  mmap/mode edits, correct next originals, duplicate/stale acknowledgements,
  process continuity, and completed-checkpoint recreation. Only this profile
  uses package-mount `dcache=0` so idle cached inodes do not prevent retirement;
  31 samples of ten small reads compare its cost with the default cache.
  Publication timings cover individual control requests, excluding Git,
  schema/hooks, and the actual helper. Automatic cleanup after retained handles
  close, snapshot reclamation, transactional recovery, and conflict installation
  over later edits remain unimplemented.
- Go experiments compile through a temporary source overlay and stay outside
  normal production verification. Their successful completion characterizes
  observed defects; it does not certify a completed redesign.
- `terminal_state_probe.ts` runs with the local Deno executable and
  `--no-config --no-lock`. It compares stock xterm serialization against
  uninterrupted continuation and records `terminal-state-results.json`; unequal
  cases prove missing recovery state, not accepted product behavior.
- `terminal-broker-benchmarks.txt` records the ordinary console package's
  `BenchmarkConsoleOutput` and `BenchmarkTerminalRead` results. These exclude
  the Deno terminal engine, browser, and real runsc throughput.
- `python3 kernel/development/analysis/terminal_probe.py` runs the stock probe,
  the isolated `xterm_state.ts` continuation prototype, and actual Chromium
  handoffs through `terminal_state_browser_probe.ts`. Raw JSON files distinguish
  stock failures from headless/browser prototype results. This does not qualify
  actual UUI sessions, pixel rendering, or agent compatibility.
- `terminal_state_bench.ts` measures headless parser throughput, idle engine
  heap, and snapshot capture/encoding/restore at two geometries. Run with local
  Deno, `--no-config --no-lock --v8-flags=--expose-gc`; raw measurements are in
  `terminal-state-benchmarks.json`. These are component costs, not retained
  Worker RSS or a full terminal workflow performance gate.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
