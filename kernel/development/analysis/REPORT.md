# Development workflow analysis

Recommendation: separate durable developer files from sandbox lifetime, and
publish immutable Git candidates without resetting the running workspace.
Persistent native terminals are already implemented; activation still destroys
their sandbox. Changing only that final restart would leave data loss and
incorrect merges in place.

The September 8 baseline refresh reproduces the failures below and adds a native
Git check. No replacement filesystem has qualified for production. The current
contract keeps immediate shared updates on untouched paths; an alternative with
explicit package synchronization is described below, but is not adopted. The
[implementation checklist](../WORKFLOW_IMPLEMENTATION.md) owns the completed
terminal phase and the hold on filesystem candidate implementation.

**Native tool compatibility is required**

The agent continues opening ordinary paths such as
`/workspace/packages/the8020/demo/main.ts`. Git, Codex, Claude Code, editors,
and build tools must use their normal filesystem operations. The filesystem owns
the choice of shared versus private storage and saves merge originals below that
interface. The agent needs no overlay API, content codec, or modified Git.
Executable loading, mmap, locks, atomic saves, links, open descriptors, and file
watching must behave correctly, including during publication. The proposed
filesystem has not yet passed that qualification; the external FUSE prototype
failed it and cannot be adopted as a general development filesystem. Reliability
and low system overhead are acceptance gates. Benchmark these native operations
under concurrent developer activity against the existing backend; the Git
preparation timings below do not establish filesystem performance.

**What fails today**

| Finding                                               | Evidence                                                                                                                                                                                                                                                                                                                                             |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The later developer silently overwrites earlier work. | Two real gVisor sandboxes edited the same file. Both activations succeeded. B replaced A's same-line edit **and erased A's non-overlapping edit**. `scanPackageChanges` selects the shared HEAD at scan time, losing the original copy-up base. The existing `TestActivationRebasesPrivateOverlayOnCurrentSharedSource` even expects this overwrite. |
| Activation kills its caller.                          | The canonical `activate` helper returned success, then its enclosing shell exited **137** before writing its post-activation marker. `resetOverlayLocked` kills/deletes/restarts; the helper only postpones it by 300 ms. Keeping the `sbx-` ID does not preserve the process.                                                                       |
| Conflict files are inaccessible to the agent.         | A competing commit after capture did reach Git's conflict path. The helper returned exit **3**, structured `conflicted` status and paths, without restarting. The annotated file existed in the host merge worktree, then cleanup deleted it; the sandbox retained its original private text.                                                        |
| Writes during activation can disappear.               | A deterministic driver-boundary injection wrote after capture, before pause. Activation succeeded; the file existed in neither shared nor private source afterward. Capture is not an atomic workspace snapshot.                                                                                                                                     |
| Abrupt sandbox loss loses source.                     | New source and an ignored artifact disappeared after killing/deleting the runtime without checkpointing and starting again. The filestore alone cannot restore the overlay namespace.                                                                                                                                                                |
| Ordinary Git state is not durable.                    | A native private commit and `analysis-local` branch disappeared even after successful checkpoint/stop/start. The edited file survived as an uncommitted modification. Checkpoint patches exclude `.git`.                                                                                                                                             |
| An untouched file can be truncated by Git.            | After a shared update, `cat` returned all 15 bytes of `shared-advance\n` while `stat` still reported 7. Path hashing matched `shared-`; stream hashing matched the complete file. Native status was empty and activation incorrectly included `untouched.txt`. This is mounted-filesystem metadata incoherence, below the activation caller.         |
| Slow activation stalls unrelated repository work.     | An unrelated repository reader remained blocked throughout an injected 200 ms schema hook; it resumed after 222 ms. The same repository mutex is shared with package administration and held across Git and hook I/O. This proves contention, not a universal deadlock claim.                                                                        |
| Activation still destroys retained terminals.         | The shared broker now retains named PTYs across connection changes. Their underlying sandbox still dies during activation; retaining terminal metadata cannot preserve those processes.                                                                                                                                                              |

The metadata failure has a concrete mount-contract cause: `developmentSpec` sets
`dev.gvisor.spec.mount.packages.share=container`, which tells pinned runsc to
use exclusive access despite the global `--file-access-mounts=shared` flag. The
source actually changes outside that container. The same hint also enables the
private self-backed overlay; changing it to `shared` alone while retaining
`--overlay2=none` would remove source isolation. Qualify independent isolation
and coherence settings at the mount owner before attributing this observation to
every gVisor mount. This does not fix missing durable namespace/Git state or the
publication races.
[Pinned mount hints](https://raw.githubusercontent.com/google/gvisor/release-20260817.0/runsc/boot/mount_hints.go),
[pinned overlay selection](https://raw.githubusercontent.com/google/gvisor/release-20260817.0/runsc/container/container.go).

The first proposed Phase 2 comparison should eliminate the simpler stock-runsc
configuration before introducing a filesystem server. For the default mount
profile, compare the current configuration with `share=shared` on packages and
`--overlay2=all:self`, plus explicit rootfs `source`, `type=bind`, and
`overlay=none` annotations to retain the ordinary durable system root. Upstream
selection handles the root override separately and skips overlays for read-only
binds; `/tmp` remains tmpfs. This is source-verified configuration behavior, not
a tested workspace candidate. The global overlay flag also affects extra
writable bind mounts, so this configuration is not a general mount-profile fix.

First require exact agreement between reads, stat, path hashing, and stream
hashing after shared file replacement, size changes, and newly published Git
objects. Private writes must never alter shared source; system/home writes must
remain durable. If those checks pass, measure the same cold/warm Git operations
as the baseline and test rename/type changes and cached open handles. Even a
pass only qualifies coherence: the private overlay namespace remains ephemeral,
and activation still needs durable Git/workspace state, real merge originals,
preservation of later writes, and package-scoped publication. This comparison
has not been run while the Phase 2 hold remains in force.

There was also a separate shared PTY bug: a blocking descriptor received from
runsc was not registered with Go's poller. Closing it left an idle read blocked,
retaining the PTY and sometimes an unreachable process. A small root-cause fix
now makes the descriptor nonblocking before `os.NewFile`. Its regression failed
before the fix and passes afterward, including race checks. The real experiment
changed from an orphaned sleeping process to proper transport cleanup. A stale
SSH test hostname expectation was corrected to use the actual sandbox ID. The
experiment's activation-command fixture was also aligned with production's
existing structured-failure contract; production already preserves that payload.

**The proposed developer flow**

1. Keep the implemented Development test terminal selector and shared PTY owner.
   Navigation, refresh, and logout detach the view. The existing 36-hour
   detached-terminal and two-hour empty-sandbox idle settings remain in force.
2. Edit private source. Untouched paths immediately follow shared publication;
   unpublished coworker changes stay private. On the first mutation of a path,
   the workspace captures its original bytes/type/mode before allowing the
   mutation. A package-start HEAD alone is insufficient because different files
   can first be edited against different shared versions.
3. Run `activate "message"`. Capture an immutable generation of selected dirty
   paths, construct a candidate with a private Git index, and merge the private
   changes with current shared source using the actual original versions. Git's
   modern `merge-tree --write-tree` supplies content merges, rename detection,
   and structured conflict stages without two checked-out worktrees.
   [Git documentation](https://git-scm.com/docs/git-merge-tree/2.39.0).
4. On conflict, publish nothing. Return exit 3 and structured paths. Put text
   markers into the corresponding private files, and retain base/local/shared
   versions in a workspace-owned conflict directory outside published source.
   Binary and structural conflicts retain all sides rather than inventing text
   markers. Install markers only if the captured generation is still current;
   otherwise retain the artifacts and report newer edits. Preserve unmerged
   state until explicit resolution; absence of marker text alone is not proof of
   resolution. After resolving, retry against the shared version actually
   presented, and merge again if another commit arrived.
5. After validation/schema preparation, publish with expected-HEAD checks under
   the shared publication owner. Use the existing deployment transaction and
   recovery records for schema/source phases. Clear only the captured private
   generations that have not changed since capture. Later edits and unselected
   packages remain private. A lost response must be queryable by activation ID
   so retry cannot duplicate a committed transaction. No sandbox pause, kill,
   remount, or delayed reset belongs in this path.

Three-way merging compares the original file, the developer's private file, and
the currently shared file. For example, suppose these settings are in separated
parts of a file:

| Version               | Timeout near the top | Retries near the bottom |
| --------------------- | -------------------: | ----------------------: |
| Original              |                   30 |                       2 |
| A's published version |                   60 |                       2 |
| B's private version   |                   30 |                       3 |
| Git's merged result   |                   60 |                       3 |

The original tells Git that B did not ask to undo A's timeout change. If B also
changed that same timeout line to 45, Git would report a conflict. Activation
would return a failure with all sides available for resolution, without
restarting the agent. Publication is the subsequent commit/update of shared
source after a successful merge and validation.

The filesystem and publisher must share the same mutation contract. A watcher
cannot atomically distinguish a clean file from a write beginning just before a
host replacement. Merely retaining an old package HEAD also misattributes newer
untouched lower content as developer work. Neither repairs first-write
ownership.

Retain a real package base tree from the start of its dirty generation, then
substitute each dirty path's observed original when constructing the synthetic
merge base. Seed it from that retained tree, not today's tree: the latter loses
shared rename ancestry and produces a false delete/modify conflict. Tests cover
both mixed first-touch versions and shared renames. Pin referenced Git objects
against garbage collection. Synthetic commits serve only the merge calculation;
the published commit descends from the actual shared HEAD.

**The simpler alternative requires a workflow decision**

If packages may synchronize explicitly, prefer ordinary private Git repositories
at `users/<user>/dev-sandbox/workspace/<namespace>/<package>/`, bind-mounted at
their normal sandbox paths. Keep each repository's complete `.git`, working
files, untracked files, and ignored artifacts there. Container recreation then
reuses the same files. A local commit records the base and native Git owns
staging, branches, renames, merges, conflict stages, and resolution.

An independent clone is preferable to a linked worktree of the shared system
repository: linked worktrees share repository metadata, while agents must not
mutate the system's refs/configuration or depend on its garbage collection. Do
not use writable file hardlinks or unpinned object alternates. Initial checkout
has a package-size space and materialization cost; reflinks can reduce data
copying where the actual storage supports them. That cost occurs on explicit
workspace creation, not every activation or sandbox start.

For process-preserving publication, capture a Git tree and publish that
candidate without resetting the active developer worktree. Later writes remain
local. Conflicts can live in a durable, package-specific ordinary Git worktree
beneath the user's workspace; the helper returns its path and exit 3. The agent
uses `git status`, edits the marked files, and stages resolutions with `git add`
or `git rm`. Retry must compare against the captured local revision and current
shared HEAD; a changed candidate requires another merge. Do not overwrite later
edits with an automatic checkout. Updating the primary worktree is an explicit
Git operation with the usual coordination between editor and Git.

This alternative sacrifices immediate shared updates in untouched files and
requires initial package materialization. It therefore changes the current
development contract and is not an accepted replacement. It is the smallest
option to qualify if that tradeoff is acceptable. Under the existing contract,
continue with a filesystem that owns first-write originals and coherent live
reads; ordinary Git behavior on that moving view remains a separate gate.

**Durable workspace storage and locking**

Keep ordinary private files, saved originals, deletions/type/mode metadata, and
conflict records under a workspace directory beneath the user's existing durable
root. Its identity must be independent of a particular runsc sandbox. Keep
mutable Git indexes, refs, locks, and newly written objects private; only
immutable shared objects may be reused read-only. Activation consumes the
workspace's dirty set, not another developer's index or a full mounted-tree
scan. The live lower view has no single historical Git HEAD: ordinary
`git status` against a fixed private HEAD can include newer shared files.
Filesystem transparency alone does not settle that Git working-tree behavior.
The helper's dirty set may guide publication, but it cannot stand in for
validating ordinary `git status`, `diff`, `add`, `commit`, and `checkout` on the
mounted workspace. Do not introduce a Git wrapper or silently rewrite a
developer's index to hide the distinction; this part of the proposed Git
experience remains unqualified.

Persist namespace changes transactionally at the mutation boundary. Save the
original before the first mutation; thereafter writes go directly to the durable
private file. Metadata can use a small transaction journal, with file/directory
fsync at its documented durability boundaries. No periodic scanner, full-tree
serialization, or per-write Git process is needed. Git ignore rules determine
publication, not whether a developer's file survives sandbox loss. Export is an
explicit copy/archive of private files, originals, and a readable manifest;
import can reconstruct text merges without the original sandbox. Full rename
behavior also requires the retained base tree/objects. Reuse the target's
package objects or include a base-only Git bundle during explicit export; that
transfer can include unchanged base content, but adds no background scanning.
Tests proved both plain-file text recovery and a transferred Git base preserving
a shared rename after the original sandbox/repository had been removed.

Use a bounded publication queue and short locks scoped to a workspace or
selected packages, in deterministic order. Perform candidate work and hooks
outside broad repository locks. Filesystem handlers must never wait for an
activation job that needs the same filesystem. Validate shared HEAD and
workspace generation again before publication; report contention or preserve
later edits, rather than retry indefinitely. Multi-package ref checks alone are
not an atomic deployment: retain the durable coordinator's recovery path.

Reusing the coordinator unchanged cannot meet the no-global-lock requirement.
`development.Activate` holds the app's shared `repositoryMu` through candidate
creation, schema hooks, and source switching. `packages.ActivationCoordinator`
also holds its own mutex through slow work, retains one `current` activation,
rejects any unfinished activation globally, and holds the PostgreSQL deployment
advisory lock through hooks and publication. The package store's existing
`lockPackage` is only process-local. These are separate ownership boundaries,
not a problem solved by replacing one activation mutex.

The shared package/deployment owner must coordinate activation, pull, checkout,
and recovery by selected package across nodes sharing the same source storage.
Prepare and validate candidates outside unrelated repository locks; retain
package-scoped mutation ownership and expected-HEAD checks through publication.
Keep durable activation/recovery records and scope them to affected packages.
Ordinary source reads and running jobs never enter publication locks. DDL still
requires the database's actual schema/table locks; bound those waits and avoid
holding them through Git preparation or arbitrary hooks. A global schema mutex
cannot be called package-contained merely because filesystem locks were reduced.
The coordinator API and catalog assumptions need an owning-layer change before
concurrent unrelated publication can be claimed.

Open writable descriptors, shared writable mmap, rename-over-save, hardlinks,
and crash ordering need explicit implementation tests. In particular, do not
delete an upper inode while a writer can still modify it. Pin writable
generations through handle lifetime and preserve writes after the captured
snapshot. A successful activation may retire only generations proven unchanged.
This is filesystem behavior, not a caller-side comparison immediately before
`unlink`.

**Terminal status**

The native PTY owner, named browser/SSH attachment, and Deno display recovery
are implemented. Their verification is recorded in the
[implementation checklist](../WORKFLOW_IMPLEMENTATION.md) and
[terminal performance report](../../../../dev-core/terminals/PERFORMANCE.md).
The September 8 native check again preserved two shell PIDs through 20
attach/detach/switch cycles and drained 4 MiB while detached. This check covers
physical PTY lifetime; it does not repeat the earlier browser/htop
qualification.

Keep that implementation. Activation must stop destroying its sandbox; a
terminal adapter cannot fix filesystem publication. Host/runsc destruction can
still end processes, but must not delete durable developer files or Git state.

**Measurements and filesystem selection**

LinuxKit 6.10.14, arm64, 6 CPUs, Go 1.26.5, Git 2.39.5, pinned runsc
`release-20260817.0`, rootless systrap. Temporary repositories reside under
`/tmp`. Git rows are medians of five trials, with one edited 171-byte file. Page
cache was not flushed. Candidate preparation includes both index
initializations, original and edited blob hashing, synthetic tree/commit
creation, merge and process startup; it excludes schema hooks and source
publication. It still processes index entries proportional to tracked file
count. The merge-only column excludes preparation.

| Tracked files | Two-worktree preparation | Its cleanup | Candidate with per-path originals + merge | Merge alone |
| ------------: | -----------------------: | ----------: | ----------------------------------------: | ----------: |
|           100 |                  12.0 ms |      1.7 ms |                                    6.6 ms |      0.6 ms |
|         1,000 |                  69.1 ms |     10.0 ms |                                   10.0 ms |      0.7 ms |
|        10,000 |                 391.9 ms |     78.7 ms |                                   23.1 ms |      0.7 ms |

Ten distinct edited 2 MiB binary files took 372 ms median for candidate creation
and merge against a common base (three trials with new blobs). Large content
still costs hashing and storage; the design removes redundant tree
materialization, not that work. Seventeen Git cases passed, including same-line
conflicts, disjoint auto-merges, identical edits, rename/edit, delete/modify,
add/add, binary/symlink conflicts, executable modes, whitespace/newline
filenames, resolution followed by another publication, mixed first-touch bases
with shared renames, stale ref rejection, and portable originals/base objects.

| Filesystem option                            | Assessment                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| -------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Existing private gVisor overlay              | Cannot satisfy durable first-write state and safe in-place retirement through its current public integration. Filesystem checkpoint/restore remains an explicit snapshot operation, not continuous mutation persistence.                                                                                                                                                                                                                                                      |
| Ordinary private Git worktrees               | Good Git isolation and normal files, but no transparent live lower view. Adding an asynchronous overwrite loop leaves a correctness race.                                                                                                                                                                                                                                                                                                                                     |
| Linux OverlayFS with live lower edits        | Reject: changing a mounted underlying layer has undefined behavior. [Linux contract](https://docs.kernel.org/filesystems/overlayfs.html#changes-to-underlying-filesystems).                                                                                                                                                                                                                                                                                                   |
| External FUSE inside pinned gVisor           | Reject for general development. The prototype proved first-write capture, private durable files, live lower reads, and retiring one upper without restarting. However, Git index mmap failed and an ELF executable failed. Scanning 10,000 small files cost **15.1 s cold / 3.59 s warm**, versus **0.545 s / 0.147 s** for the current private overlay. Warm is the median of three repeated scans. This is a minimal uncached prototype, not a ceiling on FUSE performance. |
| Host FUSE exposed through the ordinary gofer | Candidate to qualify if the immediate live view remains required. It can expose ordinary files through unchanged upstream runsc, but the filesystem must implement coherent caching, native operations, and durable mutation ownership. It needs host FUSE access and correct deployment/rootless mount handling. This environment denied opening a FUSE device, so its throughput, mmap and crash behavior were **not measured**.                                            |
| Official custom gofer extension              | Alternative if requiring host FUSE is unacceptable. The supported extension API is present in the inspected pinned-era source and supports custom LISAFS backends, but requires maintaining a custom runtime build. Preserve ordinary mmap/file-handle semantics and prevent directfs from bypassing copy-up. This integration was **not built or benchmarked**. [gVisor extension API](https://gvisor.dev/docs/user_guide/filesystem/#custom-gofer-extensions).              |

The external FUSE path can run without host `/dev/fuse`, which made the
experiment possible; its availability does not establish full POSIX
compatibility.
[gVisor FUSE transport](https://gvisor.dev/docs/user_guide/fuse/).

The storage/publication separation is the proposed direction; filesystem
selection remains conditional on qualification. Neither untested option should
be described as a measured performance winner. Before rollout, require: two
developers with concurrent saves/publications; full conflict-resolution retry;
process/cwd/TTY preservation during activation; mmap, executable, symlink,
hardlink, rename and directory behavior; crash injection at journal/publication
boundaries; export/import to a different sandbox; authentication logout and real
browser navigation/refresh; and scan/read/write latency on representative
package sizes. Source validation must consume a stable candidate through the
shared filesystem/mount owner; if a checkout is still materialized for schema
hooks, its full-tree cost remains outside the candidate benchmark above.

Persistent native terminals are complete. The current instruction holds Phase 2
until the user revises it; this refresh characterizes existing code and updates
the proposal without adopting or implementing a filesystem candidate. After that
revision, qualify the durable filesystem and Git publication in disposable
experiments. Integrate only after the correctness, native-tool, and performance
gates pass. The implementation checklist is
[WORKFLOW_IMPLEMENTATION.md](../WORKFLOW_IMPLEMENTATION.md). Keep the PTY
cleanup regression. Do not ship an intermediate activation mode that just skips
restart while retaining stale upper files, false merge bases, or lost late
writes.

Reproduction and raw observations are in [RESULTS.md](RESULTS.md),
[run.py](run.py), [runtime_test.go](runtime_test.go),
[races_test.go](races_test.go), [fuse_test.go](fuse_test.go), and
[git_probe.py](git_probe.py). The experiment code uses disposable source
overlays and does not install a new production filesystem or session feature.
