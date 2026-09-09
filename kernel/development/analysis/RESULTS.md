# Development workflow experiment results

## September 9 working prototype and conflict UI

The separate compiled prototype passes the real native browser fixture: ordinary
package creation, UUI-triggered text and modify/delete conflicts, annotated
code-editor saving, terminal takeover with `git rm`, UUI continuation, and
whole-package deletion. The development runtime stays intact. The terminal
helper reports both conflict paths, ordinary Git commands, the retry command,
and exit code 3. UUI and terminal operations use the same native worktree/index.

The native activation check also covers concurrent upstream additions during
package removal, private edits against upstream package deletion, and stale UI
save rejection. The actual schema/Worker check verifies package registration,
program execution, catalog retirement, retained table rows, and recreation of
the same package ID. These are functional checks; no new benchmark was run.

[Verification record](prototype-results.json) and
[build/use instructions](PROTOTYPE.md) describe the prototype. Optimization,
broader filesystem compatibility and production adoption remain deferred.
Historical measurements below retain their original scope and source hashes.

The final native activation check also passes ordinary link deletion/rename and
Git conflict side selection through the UUI program's sandbox helper. Link
targets are read from Git blobs, without opening the target files. The current
source repositories have no tracked symlinks; this is compatibility coverage,
not an application dependency.

The extended [standalone namespace check](gofer-namespace-results.json) fails on
an open symlink descriptor after unlink. An SDK readlink correction fixes the
earlier replaced-link failure but leaves the unlink case unresolved. The
[negative record](symlink-handle-failure.txt) retains that earlier failure. This
broader qualification is deferred; it is not a prototype delivery gate. Earlier
namespace passes below predate the added symlink-descriptor case.

## September 9 borrowed Git objects

[The failing native check](sparse-object-retention-failure.json) pruned the
shared repository's original history and then found private Git unreadable.
Private alternates now point to read-only hardlinks retained beneath the user
workspace. Initialization and selected-package preparation acquire the owning
package source lock before retaining current objects. Writable private Git and
the permanent read-only remote root use separate mounts.

The native sparse fixture verifies shared inode identity for retained objects,
including its four MiB of asset blobs. Writes and writable hardlink aliases from
the sandbox fail. After shared GC and complete repository removal, a recreated
sandbox can still read original assets and resolve an unmerged native Git
worktree with ordinary `git add`/`commit`. A replacement shared repository
appears through live source paths and ordinary fetch; private history remains
intact. Its packed object files are also retained by hardlink.

Retention does not copy object contents. It keeps object links for the workspace
lifetime, requires hardlink-compatible storage, rejects shared alternates and
promisor objects, and bounds each package's metadata walk at 100,000 entries.
Repeated shared repacks can retain older representations of the same objects;
reclamation and joint host-crash durability remain open. Whole-package
activation now builds on this retained-object owner. The
[prior cost record](activation-cost-before-object-retention.json) predates this
retention path.

## September 9 directory activation and later operations

The [native helper fixture](sparse-activation-results.json) now publishes
ordinary directory removals. An upstream edit to a privately deleted child
produces native Git modify/delete stages. Ordinary `git rm` and `git commit`
resolve that conflict; publication preserves an upstream file added to the
folder and restores its visibility in the sandbox.

Directory capture hardlinks the current removal marker. Native mkdir/rmdir
replace marker identity, so acknowledgement can distinguish a later cycle even
when it ends absent again. A lost reply leaves the attempt published; after
runtime recreation, retry finishes without another preparation and preserves
that later removal. A fresh directory-only activation uses an empty sparse Git
worktree, retires the later marker, rejects the older acknowledgement, and lets
subsequent shared children appear. Separate directory heads and small linked
receipts retain retry identity without keeping source payloads. Retirement after
held directory references close and joint host-crash recovery remain open.

The fixture also removes an upstream-tracked file under an ignored directory
while the private index is older. Ignore filtering checks only excluded file
candidates against the current shared Git tree, so either index's tracked files
retain normal activation semantics. Complete Git-object lifetime/cost
qualification remains open; package lifecycle is covered above.

Current source hashes match the passing native activation, sparse-driver, actual
schema/hook, and 77-check filesystem publication records. The latter includes
native namespace checks. No broad asset or write benchmark was repeated for this
change.

## September 9 shared-directory removal and activation error reporting

[The negative control](gofer-shared-directory-failure.json) rejected ordinary
shared-directory `rmdir` with `EOPNOTSUPP`. The filesystem now checks the merged
directory for visible children, preserves per-file originals, and records
directory removal independently. Native checks cover empty and recursive
removal, nonempty rejection, recreation with a new directory identity, retained
handles, and later shared additions remaining hidden after runtime recreation.
The namespace suite now passes 33 checks without private asset copies.

Directory records live under the private snapshots root, keyed by the path's
SHA-256 and containing that path. This keeps nested records independent without
reserving application filenames. Enumeration and lookup share the same
visibility rule. The activation change response includes directory paths; the
later capture/acknowledgement checks above now publish ordinary removals. Joint
marker/upper crash recovery remains open.

That check exposed
[an existing reporting failure](activation-error-failure.txt): the helper
returned a failed result without its reason. Disposable overlays now retain the
failure in the owning activation result and saved sandbox state; gateway
failures use the same HTTP `error` field. The command gateway preserves a
preflight error and the native helper reports conflict details. This does not by
itself cover the human CLI output or UUI editor; those now pass above.

## September 9 nested deletion and private directory removal

[The nested-deletion regression](gofer-nested-deletion-failure.json) found that
deleting `nested/remove.txt` hid `nested` itself, making its other children
inaccessible. Lookup now distinguishes actual deletion marker files from the
directories containing them. Native checks preserve two levels of originals,
leave siblings visible through runtime recreation, and observe later shared
sibling updates. The helper fixture now publishes a nested deletion and then
observes an upstream recreation of that file.

Private-only directories now use native `rmdir`, including atomic nonempty
rejection and recursive cleanup by ordinary tools. The retained-directory check
exposed [a separate SDK error](gofer-removed-directory-failure.json): lisafs
returned `EINVAL` before the filesystem could handle the removed descriptor. Its
shared handler now returns an empty enumeration for that deleted node, without
resolving the reused pathname. Native host controls on both fixture filesystems
also return an empty list after removal/recreation. The workspace filesystem
retained the old directory's link count, so link count alone cannot identify
that state.

Both native builders share the SDK overlay preparation and record both input and
patched hashes. Installed runtimes and module caches remain unchanged. The
standalone profile still omits the separate Sentry `SetStat` repair; the real
driver uses both. Native namespace and helper checks pass. Directory renames,
and joint namespace crash recovery remain open.

## September 9 shared regular-file renames

The [negative record](gofer-rename-failure.json) reproduces `EOPNOTSUPP` when
renaming between two names of the same shared inode. The filesystem owner now
recognizes that no-op and copies up ordinary renamed shared regular files using
the existing inode and original-capture path. Native rename moves the private
file; the old path retains its deletion and original for activation. Replacing
an existing destination retains that path's original too.

The [namespace checks](gofer-namespace-results.json) pass, including new and
replaced destinations, source handles following later private writes,
replaced-target handles retaining their bytes, and runtime recreation. No-op and
file-over-directory rejection leave no private state. No asset files are copied.
Directory and lower-symlink renames, interrupted namespace recovery, and broader
concurrency remain open.

The [helper fixture](sparse-activation-results.json) now renames an untouched
shared file privately, advances the original path upstream, and activates.
Native Git merges the upstream edit into the new name; validation sees the
complete candidate before publication, and both shared source and the sandbox
show the result afterward. Existing conflict/retry, later-edit, retained-PTY,
and lost-cleanup-reply checks still pass. The large-asset cost record below
predates this rename change; no broad read or write benchmark was repeated.

## September 9 captured-payload cleanup

The native activation fixture now explicitly releases its captured `file` and
`base` payloads after publication and acknowledgement. The filesystem owner
syncs the capture identity and per-path acknowledgement receipts before removing
data. Small receipts remain to reject reused IDs and support duplicate and stale
acknowledgements; the next original already has an independent copy in `base/`.
Native conflict worktrees and their Git history remain available.

The check rejects release during an unresolved conflict and verifies its bytes
remain present. Successful activation removes the captured payloads. After
runtime recreation, duplicate acknowledgement/release works and capture IDs
remain reserved, including attempts to reuse them for another path. Existing
later-edit and next-original assertions still pass.

An injected lost reply occurs after the Gofer actually releases data. The
activation journal stays `published`. A recreated runtime repeats
acknowledgement and release, finishes activation without preparing it again, and
preserves the developer's later bytes. This covers runtime recreation and an
interrupted control reply; joint filesystem power-loss recovery remains open.
There is no new background collector. Receipt retention and safe native
worktree/index cleanup remain open.

The validation view continues to use hardlinks with the measured Git read cost
below. Native `FICLONE` returned `EOPNOTSUPP` on both fixture filesystems. A
local Deno check preserved candidate-relative imports through symbolic links,
but package declaration/entrypoint validation requires real contained files. A
complete cheaper view has not been qualified; dirty-source checks remain in
force.

## September 9 single-label activation with large assets

```sh
python3 kernel/development/analysis/run.py cost
python3 kernel/development/analysis/git_probe.py hardlinks
```

[activation-cost-results.json](activation-cost-results.json) records five
sequential label edits and successful helper activations per fixture. Assets are
allocated random one-MiB files tracked in Git. The native helper, HTTP/CBus,
candidate Git operations, publication and private-file acknowledgement run with
a checking validation/completion hook. Actual schema evaluation and package
hooks are excluded. Host caches are not flushed; fixture creation primes them.
The first sample is the first activation in a fresh sandbox, followed by four
repeated activations. Setup and post-activation assertions are outside timing.

[The earlier record](activation-cost-before-release.json) preserves timings
before payload cleanup. Its storage counter used saved inode numbers without
rechecking live shared paths and could undercount recycled inodes. The current
counter verifies that identity before excluding data; a deterministic regression
simulates a recycled shared inode and requires the private bytes to be counted.

| Observation                                  |   4 MiB assets |   2 GiB assets |
| -------------------------------------------- | -------------: | -------------: |
| Edit helper median                           |      33.570 ms |      31.927 ms |
| First activation                             |   1,125.037 ms |   5,737.458 ms |
| Activation median, all five                  |   1,125.037 ms |   8,645.305 ms |
| Activation range                             | 1,107–1,150 ms | 5,737–8,893 ms |
| Initial workspace allocation                 |      0.418 MiB |      1.520 MiB |
| Workspace allocation after five              |      1.160 MiB |      3.039 MiB |
| Copied private asset files                   |              0 |              0 |
| Captured payload files after each activation |              0 |              0 |
| Host child CPU median                        |        0.339 s |        7.087 s |
| Host child block-input median                |          0 GiB |      2.004 GiB |

Edit timings include shell/runtime exec and do not include an explicit file
sync, so they are not comparable to the earlier timed native synced writes.
Allocation includes workspace/activation directories and private Git,
deduplicates inodes, and excludes unchanged shared file contents. The validation
hook checks every asset's inode; its median is about 20 ms in the large fixture.
After five large activations, private regular-file contents total 1,009,321
bytes, with a largest file of 164,334 bytes. Captured payloads are released
after every activation; small identity receipts and native worktree/index
metadata continue to grow. Their cleanup remains an open gate. Durable
system/home growth is recorded separately, and ephemeral sandbox `/tmp`
allocation is excluded.

The current run includes retained Git-object links. The initial large workspace
has 2,081 links to unchanged shared files; their contents use the existing
shared inodes, while their directory entries contribute to allocation. The
[prior run](activation-cost-before-object-retention.json) measured 1.17/7.99 s
activation medians before retention. These sequential runs do not isolate its
latency cost; no copied asset payloads appear in either run.

The large fixture's waited host-child block-input counters range from 1.724 to
3.434 GiB per edit/activation; their block-output counters range from 368 to 524
KiB. Live kernel, Gofer and Sentry counters are recorded separately, with no
combined I/O total. Sampled post-activation RSS ranges are 13.2–15.0 MiB for the
kernel test process, 13.3–15.7 MiB for Gofer and 29.1–32.6 MiB for Sentry. These
are samples, not peak memory or transient host Git RSS. The run uses
Linux/arm64, Go 1.26.5, the recorded custom gVisor SDK revision and `/tmp`
OverlayFS (`0x794c7630`). Source and SDK digests identify the tested inputs.

The [native Git control](activation-link-read-results.json) identifies a cause
of the excess reads. Creating validation hardlinks changes source ctime while
leaving mtime and content unchanged. Git 2.39.5 reports four content scans for
four linked assets, then zero after refreshing its index. Removing those links
causes another four scans. Git records this counter when refreshing an entry
requires its modification check.
[Git refresh implementation](https://github.com/git/git/blob/v2.39.5/read-cache.c#L1385).
The current activation view creates and removes these links around each
validation. This isolates an owning validation-path cost; it is not a reason to
disable dirty-source checks. No asset copying occurs, but the publication I/O
cost is not yet qualified.

The separate native conflict and actual schema fixtures also pass with the
updated fixture inputs. Their helper timings below were refreshed on September
9; earlier transaction/race records retain their original input digests.

## September 8 activation identity and late completion

```sh
python3 kernel/development/analysis/run.py transaction
```

The [negative regression](transaction-failure.json) completes activation A,
prepares B without switching B's source, then retries A's completion. The old
shared `Complete(true)` consumes the current transaction and publishes B's
commit as ready. This is a shared handshake defect, independent of the UI or
terminal caller.

The [candidate patch](activation-transaction.patch) requires an explicit `act-`
ID for package/schema preparation and completion. Existing development,
synchronization, repository mutation, deletion, bootstrap and recovery callers
use that identity; the pending schema record retains it. Completed/failed
transactions accept only a matching terminal outcome. Unknown successful
completion and reused IDs are rejected. Aborting an ID whose preparation never
started has no work to undo; callers must never reuse an abandoned ID. Recreated
completion owners acquire exact activation ownership before reading transaction
state.

[transaction-results.json](transaction-results.json) records the repaired
regression and existing package, database, evaluator, development and deployment
checks. The runner applies the patch to copied compiler inputs; production
sources and installed runtimes remain unchanged. The concurrency checks below
cover the prototype coordinator/evaluator; concurrent PostgreSQL activation and
interrupted source switching remain unqualified.

The real schema/helper fixture below also uses this patch. After A has completed
its database work but before the helper finishes, B prepares through the actual
evaluator and pre hook. Retrying A preserves B's prepared stage, previous active
commit and exact pending schema ID. Only B's own explicit rollback clears it.

The [rollback regression](transaction-rollback-failure.json) found a cleanup
failure marked terminal, so the next retry silently skipped it. The patch now
retains an incomplete stage until rollback succeeds and propagates rollback
errors from preparation. The same root check retries a deliberately failed
rollback and verifies that cleanup runs again.

The [preparation regression](preparation-recovery-failure.json) records the
missing durable preparation phase and rejected abort of an unstarted identity.
The activation journal now enters `preparing` before calling the shared owner.
Retry settles that exact preparation before rebuilding validation and starting a
fresh transaction, retaining the original captured files and native Git work.

The [catalog concurrency regression](concurrent-catalog-failure.json) found that
the database rejected a second pending deployment even for an unrelated package.
The patch now admits disjoint package sets and rejects overlapping ones within
the database transaction. Completion looks up its exact ID and merges only its
package changes into the latest catalog, preserving independent publications.
The regression completes B before A, checks both commits and pending status,
then publishes a removal while rolling back another package's update. The
catalog hash remains valid throughout.

This prototype uses a bounded pending-record scan, with at most 256 deployments
and 256 candidate packages per deployment; normalized package claims are the
upgrade if that ceiling becomes relevant. These are SQLite catalog checks.

The [coordinator regression](concurrent-coordinator-failure.json) pauses the
Orders pre hook and tries to activate Invoices on the same coordinator. The old
mutex holds Invoices until Orders resumes. The repaired check completes Invoices
while Orders is paused, rejects an overlapping candidate through another
coordinator, and rejects concurrent completion of the still-executing Orders
prepare. A recreated coordinator then completes Orders without changing
Invoices.

The evaluator owner test also pauses Orders during table evaluation, completes
Invoices, then rolls Orders back through a recreated evaluator. Invoices and an
existing Orders row remain intact. The coordinator and evaluator retain no
single pending attempt or lock across the prepare/complete interval. Each call
uses an exact-ID operation lock with nested context ownership and idempotent
release. Durable unfinished package rows prevent overlap between calls.
Admission alone uses a short global metadata lock and the existing stage index,
with at most 256 unfinished activations and 256 candidates per activation.
Ordinary hooks and evaluation run outside that lock; bootstrap/full schema
synchronization remain exclusive.

The [focused race results](transaction-race-results.json) cover these
coordinator/evaluator cases and exact operation ownership. PostgreSQL uses
per-ID advisory locks in the patch but has no live qualification yet.

The [source ownership regression](source-ownership-failure.json) pauses an
actual checkout after preparation returns and before directory replacement.
Recovery previously rolled it back as interrupted, leaving the live publisher
unable to finish its transaction. The shared package owner now supplies sorted,
nonblocking native locks under `packages/.meta/activation-locks/`. All existing
package mutation callers, both development activation implementations and
recovery use those locks across preparation, switching and completion. Lock
files remain outside package directories and are never removed during package
deletion. The broad package-store mutex and process-local lock map are removed.

The repaired regression preserves the paused checkout while another package's
checkout completes. Child processes verify exclusion, unrelated progress,
partial-acquisition cleanup, directory-replacement stability and release when
the owning process is killed. These cases also pass the race detector;
application composition compiles against the changed package configuration. This
qualifies separate processes on the fixture filesystem, not a multi-node
PostgreSQL/shared storage deployment. Startup's handling of a live pending
activation and runtime-index concurrency remain open; full concurrent activation
is unqualified.

The [publication regression](publication-index-failure.json) fails ordinary,
bootstrap and recovery paths when index refresh fails after publication: the
activation is already terminal, so retry never refreshes the index. The
prototype now records `published` atomically with package records and the
package revision. The shared finalizer refreshes indexes and removes source
backups before marking completion. Failed refresh or completion-record writes
retain the published stage and error; rollback and overlapping package
activation are rejected. Recreated completion/recovery performs no second schema
completion, hook execution or revision increment. The existing
atomic-publication check now injects failure at the `published` record in that
same transaction; the new regression separately rejects final completion.
Cleanup also accepts absent namespace directories when deleting an uninstalled
package.

The matching [stage definition patch](activation-stage.patch) changes only the
copied packages table used by the native fixture. Production contracts remain
unchanged while this design is qualified. Coordinator retry ownership and
runtime convergence are verified separately below.

The [batch recovery control](batch-recovery-failure.json) substitutes the former
single-attempt dispatcher while retaining current recovery safety checks. With
one live publisher followed by two abandoned attempts, it reports the live lock
and leaves both abandoned preparations pending. The repaired owner snapshots at
most 256 unfinished IDs, closes its SQL cursor before native work, and visits
every attempt under its own source/operation ownership. Busy and failed attempts
remain errors, but they no longer prevent the other recoveries. Releasing the
live owner allows a recreated coordinator to finish its remaining rollback.

The startup overlay now consults activation records independently of pending
schema records and inspects source commits after recovery, avoiding a stale
pre-recovery comparison. A final full index includes records restored after an
earlier recovery callback. The app checks cover the copied composition, but a
real node boot during active publication is still unqualified. That case still
fails on busy recovery and the exact installed/ready-catalog comparison.

The [published-set regression](published-snapshot-failure.json) independently
fails when initialization combines old filesystem commits with a newer revision,
and when a package preparing its next activation loses its previous published
identity. The shared index now reads revision and published commit rows together
in one SQL statement. A preparing or failed package with an active commit
remains published; retired packages and rows without an active commit are
excluded. The follower captures this baseline before the full initial index
instead of reading a revision after an earlier filesystem scan. Its existing
cheap-poll regression still permits only one scalar query when nothing changes.
Owner/regression race checks and app tests pass against the copied inputs.

The [availability regression](preparation-availability-failure.json) reproduces
six effects of marking an existing package unavailable after preparation:
package resolution, program resolution, program selection, exact unchanged
source verification, another package's handler indexing, and another package's
activation hook fail. Removing the shared disable step preserves the existing
ready record and active commit. The pending activation retains its own stages,
errors and overlap claims. New packages still have no ready version until their
first publication. Existing completion/recovery checks retain the previous
active identity after post-hook or publication failure; only successful
publication advances it. Ordinary source reads retain the agreed mutable-source
semantics, while exact-source schema verification remains unchanged.

The [indexing control](runtime-indexing-failure.json) pauses a provider for one
package and demonstrates another request waiting behind its global lock. The
repaired indexer holds its memory lock only to claim package IDs and maintain
pending work. Queries, provider jobs and runtime actions run outside it. A newer
request during an older job remains pending after that job succeeds; retry reads
the updated provider configuration. Busy owners are skipped during ordinary
retry, and unrelated requests proceed independently.

The [outer-consumer control](revision-consumer-lock-failure.json) then repeats
the failure through `runtimeSharedState` with the real SQLite index follower.
Removing that execution lock exposed an
[acknowledgement failure](revision-acknowledgement-failure.json) when revision 1
completed after revision 2. Both followers now ignore older and duplicate
completions, leave newer pending state intact, and reject zero/future
acknowledgements. Package/app race checks pass, including the full consumer call
and retained newer requests. Source-update restart intents already reject older
update identities at their shared owner.

The [periodic-monitor control](revision-monitor-failure.json) pauses the same
kind of provider through the actual monitor loop and SQLite revision follower.
Before the repair, a newly published second package cannot reach indexing until
the first provider returns. Periodic refresh now queues service work through the
same indexer. The revised check delivers timer ticks directly and observes the
second provider, database failure gating, and recovery while the first provider
remains paused. Repeated polls do not duplicate that running provider.

A companion owner check fills the 16-background-batch limit, retains the next
package for retry, releases capacity and publishes that package, then cancels
and joins a running provider at shutdown. Closed owners reject new background
work. Provider work uses a runtime-owned context with the ordinary five-minute
deadline; returning from a polling call does not cancel its queued work. Native
declarations still refresh before revision acknowledgement. Explicit command and
activation paths retain synchronous indexing.

These are in-process checks with provider and health doubles, not native Deno
indexing or a full node boot.

The [declaration-lock control](declaration-lock-failure.json) holds an inspected
old program/package value while another refresh starts. Both owning indexes
previously blocked that second refresh. They now read outside the publication
lock and compare a counter before replacing their cached catalog. If another
publication intervened, they rebuild from the latest snapshot. The separate
[ordering control](declaration-order-failure.json) disables only that comparison
and loses newer event/program references and command fragments, confirming that
unlocking alone is insufficient.

The handler check uses actual SQLite metadata and native declaration/program
validation. It updates the target of cached cross-package references while an
older lookup is paused. Command checks use real manifests and the existing
metadata double; they cover unrelated packages, the same package, and a full
rebuild completing after a scoped update. Both handler kinds stay atomic, and
the existing command collision checks and scoped-discovery checks remain in the
owner suite. Publication locks now cover memory snapshots/replacement only.
Concurrent publication may repeat selected reads until the caller's deadline;
high-contention throughput remains unmeasured.

Package-path comparison still retains its follower lock across Git. Complete
runtime concurrency and live startup remain unqualified.

## September 8 actual schema, hooks, and completion retry

```sh
python3 kernel/development/analysis/run.py schema
```

[sparse-schema-results.json](sparse-schema-results.json) records the additional
native job/Worker runtime, Deno table evaluator, SQLite schema engine, and
package activation coordinator check. The same sparse driver and disposable
activation owner run through the ordinary authenticated helper and command bus.
The service runtime uses a previously materialized image read-only; its record,
prototype source digests, original package-input hashes and final staged Git
trees are retained. The packages table receives the recorded stage-definition
patch before initialization. The common fixture also stages the actual
`dev-skills` package for the current required mounts and records its Git tree.
Package staging includes current nonignored source files and preserves modes,
excluding Git metadata and environment files. Four one-MiB random assets remain
in the development package fixture.

The first candidate changes an existing column default and adds a nullable
column. The actual schema engine rejects the unsupported default change. Shared
HEAD and private edits remain intact. An ordinary edit/add/commit in the saved
Git candidate keeps the old default while retaining the new column. Activation
then applies that column, preserves an existing data row, and runs the actual
pre/post hooks in native Deno Workers. Both hooks verify the new table source.

An injected error after database completion stops private-file acknowledgement.
The saved attempt remains published. Newly constructed evaluator/coordinator
objects continue it through the helper; activation A's hooks each have one
execution attempt, the activated catalog matches shared HEAD, and the
development runtime is not reset.

| Helper observation                                        |         Time |
| --------------------------------------------------------- | -----------: |
| Incompatible schema rejection                             | 2,315.654 ms |
| Corrected publication through database completion         | 2,009.609 ms |
| Retry after completion, including private acknowledgement |    92.126 ms |

These are one observation each on Linux/arm64, Go 1.26.5, Deno 2.9.4, Git 2.47.3
and `/tmp` OverlayFS. Timings include helper startup, HTTP/CBus, native Git and
the relevant database/Worker operations; initial source staging, database
bootstrap and runtime setup are excluded. The second observation deliberately
returns failure after database completion, so it is not a successful full-helper
latency. Registration, heartbeat and database-scope callbacks are fixture
acknowledgements; table evaluation, SQL and hooks are real. This is object
recreation after an injected error, not a process/host crash. PostgreSQL
concurrency and full storage/I/O costs remain unqualified.

The same run exercises four interruption boundaries with edits in two packages:

| Interruption point             | Prior transaction outcome | Recovery helper |
| ------------------------------ | ------------------------- | --------------: |
| Before preparation starts      | No database record        |    2,379.248 ms |
| After preparation completes    | Rolled back               |    2,599.333 ms |
| Before the last package resets | Rolled back               |    2,523.772 ms |
| After the last package resets  | Completed                 |      224.114 ms |

The first three cases prepare a fresh transaction for the retained native Git
candidate. A partial source switch first restores the changed package to its
previous clean commit. When all sources already match the candidate, recovery
finishes the existing transaction; the pre and post hooks each run once for that
ID. Each case verifies exact source/catalog commits, preserved later private
edits and the same development runtime. The partial-switch case also introduces
an unrelated shared edit: retry refuses it without changing its bytes, then
continues after the test removes its own edit.

These are single helper observations at clean package-reset boundaries, using
recreated coordinator/evaluator objects. The expanded fixture allows 64 logging
producers to cover its retained candidate sandboxes; log storage remains bounded
to eight MiB. Interruptions within a native reset that leave dirty trees remain
ambiguous and fail closed; host/kernel crash qualification is still open.

The initial test image expected the removed runtime-group identifier; selecting
the current-node-protocol fixture image repaired setup without runtime changes.
Those checks exposed missing error detail when preparation fails. The shared
activation result and prototype transport now retain that cause for the readable
helper and UUI.

An additional activation fails its handler-index refresh after real source,
schema and hook publication. Both the native attempt and database activation
retain the published phase. The developer then edits the same file again. Retry
through a recreated coordinator/evaluator successfully runs ordinary handler
indexing, leaves the package revision unchanged, and retains exactly two hook
attempts. Shared source contains the captured text, the workspace contains the
later edit, and the existing sandbox remains alive. The raw
`index_completion_retry` observation records both helper timings. This exercises
the actual handler owner with an injected first-call error; it does not qualify
the app's complete command/service-index or startup/concurrency path.

The native fixture also prepares three actual schema transactions and holds the
first package's source ownership. Recovery preserves that live attempt while
rolling back both later abandoned preparations, including their exact pending
schema rows. A recreated coordinator then recovers the first after ownership is
released. `batch_recovery` records the two call durations, including native
schema rollback and excluding preparation/helper transport. This exercises the
same coordinator used by startup, not the complete app initialization path.

The published-revision check creates a follower before bootstrap, observes and
acknowledges bootstrap publication, then follows A's actual changed source paths
while B is preparing the same package. It must retain A's published commit and
report the changed table file rather than a package removal. Acknowledgement and
B's rollback produce no phantom publication. This covers native Git comparison
and the real schema/package owner, not a whole node boot during live
publication.

While B remains prepared, the same fixture resolves a program from A's published
commit and runs it through the ordinary program runner, native job and Deno
Worker. Its returned value and allocated Worker ID prove the existing program
remains callable. This invocation occurs outside the measured helper intervals.

## September 8 actual helper conflict, publication, and failure retries

Reproduce the disposable activation owner through the canonical script:

```sh
python3 kernel/development/analysis/run.py activation
```

[sparse-activation-results.json](sparse-activation-results.json) records the
source and SDK digests, environment, explicit unqualified gates, and results.
The fixture uses the real Manager/RunscDriver, authenticated HTTP endpoint,
command bus, native sandbox Git, and retained terminal broker. A Go source
overlay replaces `Manager.Activate`; installed code is unchanged. Linux/arm64,
Go 1.26.5, Git 2.47.3, Deno 2.9.4, and the pinned generated SDK with the two
disposable SDK fixes run on `/tmp` OverlayFS (`0x794c7630`). Four allocated
one-MiB random assets are tracked in Git.

The helper now produces a retained native sparse conflict worktree. Its three
Git stages contain the actual observed original, captured private content and
shared content. Originals may come from different shared revisions in one
package. Git resolves disjoint changes and reports the overlapping label.
Markers/index stages survive driver recreation, and ordinary edit/add/commit
plus helper retry preserves another shared advancement. Captures remain fixed
while the agent continues editing its primary workspace. Publication retains
those later edits, ignored artifacts and the existing Bash PID.

The checking schema hook verifies the complete candidate before shared source
changes and completion after publication. Unchanged candidate assets share
native source inodes. A new edit while validation holds those hardlinks remains
private; visible lower aliases share their private inode across recreation,
while host-only aliases and names beneath the lower `.git` directory shadowed by
the private reference file remain shared. The prototype walks at most 100,000
lower metadata entries to find visible aliases. This is not a qualified
large-tree/concurrent-write cost.

Deleting existing files, recreating a captured deletion during validation, and
later upstream recreation of a published deletion pass. A negative control found
that deleting a captured new file was lost at publication because no shared
counterpart existed yet. The owning filesystem now records absence independently
of that counterpart. The same helper regression passes, including the next
activation's deletion. Pending capture registration also survives a failed
recapture, and ordinary atomic-save temporaries leave no deletion markers. See
[the recorded failure](sparse-new-deletion-failure.txt).

Schema rejection leaves shared source unchanged and retains the attempt,
resolution, and later private edit. Another failure injects a shared commit
during validation; expected-HEAD verification rejects publication and completes
the schema preparation with `false`. Retrying the same attempt merges that new
commit, publishes the native resolution, and preserves later private work.

| Helper observation                            |         Time |
| --------------------------------------------- | -----------: |
| First activation reporting the label conflict | 1,503.563 ms |
| Publication after native resolution           |   684.408 ms |
| Publication with a lost release reply         |   493.704 ms |
| Retry after the lost release reply            |   106.761 ms |

These are single observations of different scenarios, not distributions. They
include helper startup, HTTP/CBus/Manager, Git, native candidate preparation and
the checking hook. They **exclude the actual schema engine and package hooks**.
Single-label costs with larger asset trees are recorded above. Full activation
costs with actual hooks and multiple developers, full borrowed-object lifetime,
snapshot cleanup, interrupted source/schema recovery, and full native namespace
semantics remain unqualified. The earlier component write medians below are from
their identified earlier revision.

The human UUI screen now uses the existing code editor and this same native
conflict state, with file selection, labelled/highlighted sides and
continuation. The compiled prototype check above covers UUI/terminal handoff and
readable helper output.

## September 8 real driver, Git metadata, and helper preview

The sparse filesystem now runs through the real development Manager/RunscDriver,
authenticated helper endpoint, command bus, and retained PTY broker. Reproduce:

```sh
python3 kernel/development/analysis/run.py sparse
python3 kernel/development/analysis/run.py sparse unpatched
```

The first command must pass; the second is a negative control that must fail
when direct `utimensat` incorrectly returns success. These runs use disposable
source overlays and fixtures, not an installed runtime or production change.
[sparse-runtime-results.json](sparse-runtime-results.json) records source
digests and exact SDK before/after digests. This fixture uses Linux/arm64, Go
1.26.5, Deno 2.9.4, Git 2.47.3, and the same pinned generated gVisor SDK. Its
`/tmp` host filesystem is OverlayFS (`0x794c7630`), so absolute timings are not
directly comparable with earlier workspace-mounted fixtures. Four allocated 1
MiB random assets are **tracked in Git**.

The canonical helper script is unchanged. Two compiler overlays repair owning
boundaries: Sentry returns shared-mount `setStat` failures, and the activation
scanner refreshes clean index entries before staging a newly initialized index.
The current fixture also applies the shared transaction patch for source
ownership. Git gets private native metadata initialized by
`clone --shared --no-checkout`, with a read-only retained-object alternate. This
creates no source checkout or private object-content copy. A standard `.git`
file points to `/workspace/git/private/<package>/.git`, with one private
metadata mount outside the source tree. The retention checks above cover
GC/removal/recreation; multi-package initialization at scale and complete object
lifetime remain open.

The first source edit preserves its parent directory's device/inode/mode and the
Git reference. Helper preview reports exactly the label change. A filesystem
capture precedes a later edit and subsequent Git preparation, which uses only
captured bytes. Native Git creates a sparse conflict worktree with diff3 markers
and exact base/private/shared index stages. All survive driver
kill/delete/recreation. Ordinary editing, `git add`, and `git commit` resolve
the conflict; another merge preserves that resolution after shared code advances
again. No assets are materialized in the conflict worktree.

An incremental native bundle carries the resolved commit to the trusted shared
fixture. Publication plus filesystem acknowledgement preserve the later primary
edit and the retained Bash PID. Another recreation preserves the resolved
worktree, the private branch, later source, and the correct next original. This
publication is still test-driven and bypasses schema/hooks.

The [former Git mount failure](sparse-gitdir-mount-failure.txt) records native
rename returning `EBUSY`. Reference-file rename now succeeds. Removing that file
and recreating the sandbox leaves it absent; Git still reads private history
through its explicit metadata path. Restoring the reference reconnects the main
worktree, and the resolved conflict worktree remains intact. This qualifies the
reference layout, not whole-package removal or interrupted initialization.

| Observation                                        |        Result |
| -------------------------------------------------- | ------------: |
| Initial private regular-file content               |   4,866 bytes |
| Content added by first label edit and its original |      19 bytes |
| Helper preview, including runtime exec/startup     |    224.898 ms |
| Preview Sentry write syscalls (`wchar`)            | 420,457 bytes |
| Preview Sentry storage writes (`write_bytes`)      | 536,576 bytes |
| Incremental resolved-commit bundle                 |   1,054 bytes |
| Final private regular-file content                 |  10,800 bytes |
| Final allocated regular-file storage               |       272 KiB |
| Copied assets, including large private Git blobs   |             0 |

These are single integration observations, not latency distributions. Process
I/O comes from `/proc/<pid>/io` deltas for the Sentry and Gofer; it includes
helper, Git, control traffic, and caches. It is not a pure source-write
benchmark. Final storage deduplicates regular-file inodes and includes Git
metadata, originals, captures, and conflict files; unchanged shared Git inodes
and directory allocation are excluded. These totals precede the GC/removal
checks above, after which the retained history can outlive its shared names.
Temporary host validation checkouts in the current activation failure path are
also excluded. The assertions inspect all private object files and reject
asset-sized preview I/O, not merely the absence of files under `assets/`.

The cost checks exposed three distinct failures:

- A rejected metadata-only operation first copied asset blobs into original and
  upper storage. The Gofer now rejects wholly unsupported masks before copy-up.
  The initial [storage record](sparse-storage-failure.json) contains about 8 MiB
  of copied Git blobs despite zero private `assets/` entries. Its earlier
  success flag predates this assertion and does not qualify small-edit storage.
- Correct rejection exposed Sentry returning false success on shared mounts. The
  one-line owning fix is applied in the real-driver profiles; the
  [unpatched negative control](sparse-setstat-failure.txt) reproduces the
  defect. Standalone probes still omit that Sentry fix; current builds include
  the separate removed-directory handler fix described above.
- Read-only existing objects, including alternates, cannot have their timestamp
  refreshed. Git then writes a private object. A fresh `read-tree` index had
  forced unchanged assets through this path. The
  [earlier I/O record](sparse-git-io-failure.json) and
  [alternate-store failure](sparse-alternates-failure.txt) show 4,763,648 Sentry
  storage-write bytes for the preview. Refreshing the scanner's clean index
  entries first removes these asset writes; dirty entries still reach normal
  staging. This does not guarantee that every arbitrary Git command avoids
  object copies.

Separately, creating upper parent directories changed their visible inode/mode,
which detached the private Git mount and broke conflict recovery. The
[failing regression](sparse-directory-failure.txt) records that loss. Lookup now
keeps a lower directory's identity and metadata when its upper directory exists
only to hold private children. General directory mutations remain unqualified.

The actual helper's conflict invocation returns exit 3 and structured conflict
paths, but its existing publication owner still removes the conflict worktrees.
That negative result is recorded separately; its duration is not a successful
activation benchmark. Joining capture/native resolution/publication to this
owner, schema-before-source coordination, interrupted-transaction recovery,
automatic retirement, full shared-object lifetime, and broader Git/filesystem
semantics remain open. **The full goal remains active.**

The focused standalone namespace regression also passes its 23 checks with the
directory fix; [its refreshed record](gofer-namespace-results.json) identifies
the tested source. This uses unpatched Sentry and does not qualify
metadata-error propagation. Earlier read/write/publication records retain their
own source revisions and are not relabeled as current measurements.

Verification passes the real-driver fixture, the expected failing unpatched
control, 23 namespace checks, current positive-result source digests, unchanged
SDK inputs, reproducible compiler overlays, Python syntax, Go/Markdown/JSON
formatting, and whitespace checks. DOX verification passes 211 documents and 17
complete roots, with exact child coverage, reciprocal parents, relative links,
and reachability. Analysis docs and the implementation checklist are updated;
broader development/root contracts are unchanged because this work only
qualifies the prototype and does not change production behavior or requirements.

## September 8 publication capture and retirement

That recorded prototype revision passed 67 component checks, including the
earlier mounted native Git conflict/resolution cases. Reproduce with:

```sh
python3 kernel/development/analysis/gofer_probe.py publication
```

[gofer-publication-results.json](gofer-publication-results.json) records the
then-tested Go/Python/native-client/Git-helper source hashes, commands, checks,
and raw samples. This revision uses the same pinned SDK and workspace-mounted
host filesystem as the earlier probe, with four allocated 1 MiB assets and ten
tracked fixture files. Asset contents remain outside the Git index. It uses a
private control socket between the fixture publisher and confined Gofer; agents
still edit ordinary files. This is a filesystem-owner experiment, not a new
agent command or the production activation implementation.

The filesystem captures the original and private file before publication. Native
Git merges that capture with a subsequent, non-overlapping shared edit in the
trusted fixture. Acknowledgement retires an unchanged private file, returning
new reads to the merged shared source. Later atomic saves, permission changes,
writes through retained descriptors, and writes through a live mmap after its
application descriptor closes stay private. A second publication preserves both
the shared change and those later edits. Its original is the previously captured
private version, not the merged shared version that the private file never
contained. Duplicate acknowledgements do nothing; stale acknowledgements cannot
retire newer work. Old captures remain readable from the confined Gofer.

The same background shell, cwd, and retained read descriptor survive these
publications. After a completed acknowledgement retaining a later edit,
kill/delete/recreation preserves that edit, the next original, the capture, and
the acknowledgement identity. This tests runtime loss after completed
operations, not interruption inside a transaction, host power loss, the retained
PTY service, or the actual activation script.

| Filesystem control request        | Samples |  Median | Observed range |
| --------------------------------- | ------: | ------: | -------------: |
| Capture                           |      10 | 4.70 ms |   3.53–5.47 ms |
| Acknowledge                       |      10 | 3.77 ms |   3.46–4.40 ms |
| Exclusive part of acknowledgement |      10 | 1.47 ms |   1.21–1.64 ms |

These are heterogeneous functional samples on approximately 174-byte text files
and a 4 KiB mmap file, with debug logging enabled. Capture/acknowledgement
timing includes the local socket round trip and filesystem work, excluding Git,
schema/hooks, and runtime startup. The exclusive duration starts after acquiring
the per-workspace lock; it excludes waiting for that lock, file copying/hashing,
Git, and shared publication. This component profile excludes the actual helper;
the separate helper observations above use a checking schema hook.

The initial retirement check exposed idle Sentry inode caching: even closed
application files retained a Gofer reference. The publication profile now uses
the SDK's existing package-mount `dcache=0` option. Other mounts keep their
cache policy. Actual open handles and mappings still protect private files. This
change justified one small read comparison, without repeating large-tree reads:
31 samples each read ten small files inside one native executable, after a
warm-up, with debug logging and runtime startup excluded. Default caching took
8.01 ms median/9.40 ms p95 per ten files; `dcache=0` took 12.81/13.40 ms. The
observed median difference is 4.79 ms per ten files (60%), with sequential
profiles and uncontrolled host scheduling. These latest samples accompany the
directory-publication regression run. The
[earlier samples](gofer-directory-read-before.json) measured 10.99 ms median
with `dcache=0`; these repeated observations do not isolate a causal cost.
Earlier broad-read and write timings use earlier revisions/cache settings; they
do not measure this publication profile.

Replacing a previous captured-file hardlink in the next-original slot exposed an
old-capture open failure in the confined Gofer, despite host readability. The
owner now installs an independently copied, synced file at the next-original
path using the existing atomic-copy helper. The old-capture checks pass; a small
host-only hardlink reproduction did not reproduce the confined failure. General
hardlink semantics remain unqualified.

The first label edit still allocates 8 KiB for 20 logical bytes. After the whole
publication/conflict sequence, 94 distinct private regular inodes contain 37,532
logical bytes and occupy 352 KiB, including private Git state, originals, and
retained publication captures. Zero assets are copied. Symlinks, deletion
markers, and snapshot entries are counted separately; directory allocation is
excluded. Captures currently accumulate without reclamation.

An unchanged file held open during acknowledgement remains private after the
handle closes until a later publication rechecks it. Automatic retirement on the
last close is missing. Atomic recovery across originals/private files/deletion
markers/acknowledgement records is also missing; the acknowledgement identity
checks do not establish crash-safe publication. Actual helper/schema/hooks,
private Git ownership, concurrent conflict installation, general namespace
operations, watcher behavior, and deployment profiles remain qualification
gates. The full goal stays active; production behavior is unchanged.

Verification covered the actual SDK build and 77 checks, with source hashes
identifying the tested revision. Analysis DOX and the implementation checklist
record this component progress; broader development and root contracts remain
unchanged because their required outcomes and production ownership have not
changed.

## September 8 namespace, writes, and native conflicts

The user accepts the measured read overhead for the prototype and prioritizes
the ordinary edit/activate/conflict/resolve/retry loop. Further read-performance
tuning is not required by the present results. Reproduce the focused checks:

```sh
python3 kernel/development/analysis/gofer_probe.py writes
python3 kernel/development/analysis/gofer_probe.py atomic
python3 kernel/development/analysis/gofer_probe.py namespace
python3 kernel/development/analysis/git_probe.py conflict
```

The earlier write revision records
[gofer-write-results.json](gofer-write-results.json) without replacing the
earlier large-tree read record. It runs 23 functional checks with four allocated
1 MiB assets, then measures first/repeated overwrites of 31 distinct files at
each size in each backend. There is no new large-tree read benchmark. Timing
runs inside the sandbox and includes open/truncate/write/file-sync/close,
excluding executable/shell startup and fixture preparation. The first sparse
save also captures and syncs the original and creates/syncs the initial private
copy. Complete copies are linked into place after file sync, followed by their
parent-directory sync; this additional work changes first-save timing from the
earlier transport-only revision. Repeated overwrites reuse that private file.

|     Size | Sparse first save | Sparse repeated save | Native directfs first save | Native directfs repeated save |
| -------: | ----------------: | -------------------: | -------------------------: | ----------------------------: |
| 64 bytes |           4.44 ms |              0.79 ms |                    0.36 ms |                       0.32 ms |
|   64 KiB |           4.70 ms |              0.82 ms |                    0.39 ms |                       0.34 ms |
|    1 MiB |           8.86 ms |              1.55 ms |                    1.07 ms |                       1.10 ms |

The table contains medians. Raw samples, p95 values derivable from them, and the
stock-RPC control remain in the JSON record. Final bytes of every file, every
sparse original, and unchanged sparse source files are checked; all 26 component
checks pass. Profiles run sequentially on the same workspace-mounted host
filesystem as the read probe, using separate file sets and uncontrolled host
scheduling.

[gofer-atomic-results.json](gofer-atomic-results.json) uses the same sizes and
31 samples per phase/profile. Each sample creates a temporary file, writes,
syncs and closes it, renames it over the target, then opens/syncs/closes the
parent directory. Timing is inside the sandbox with debug logging off. First
replacement captures the target's original without creating an unnecessary
private copy of its old contents. Repeated replacement retains that same
original. All 26 checks pass.

|     Size | Sparse first atomic save | Sparse repeated atomic save | Native directfs first | Native directfs repeated |
| -------: | -----------------------: | --------------------------: | --------------------: | -----------------------: |
| 64 bytes |                  8.14 ms |                     5.84 ms |               1.17 ms |                  1.20 ms |
|   64 KiB |                  8.30 ms |                     5.83 ms |               1.22 ms |                  1.16 ms |
|    1 MiB |                 10.67 ms |                     6.58 ms |               1.86 ms |                  1.78 ms |

[gofer-namespace-results.json](gofer-namespace-results.json) now runs 30
functional checks alone, including the September 9 rename additions above.
Native sandbox Git 2.47.3 stages and commits without changing shared HEAD. After
a shared commit, ordinary `git merge` returns 1 with diff3 markers and exact
original/private/shared index stages. Killing/deleting/recreating the runtime
preserves those markers and stages; ordinary editing, `git add`, and
`git commit` resolves them. Another recreation preserves the resulting merge
commit, atomic save, deletion, and ignored files. The existing `native_probe.go`
passes private hardlink/symlink behavior, open writable handles across atomic
replacement, mmap, flock, and local inotify. Deleted lower names disappear from
both lookup and enumeration. The initial label still copies only 20 logical
bytes/8 KiB allocated, with no assets copied. After all namespace/conflict
operations, the recorded private regular inodes occupy 184 KiB in the expanded
fixture, including private Git state and originals. Storage totals deduplicate
hardlinks, count symlinks/deletion markers separately, and exclude directory
allocation. This small Git fixture tracks four source files, not the asset
bundle; it does not measure asset-heavy Git indexes or history.

Completed-write runtime recreation is tested, not host power loss. The prototype
still lacks joint original/upper/deletion recovery, directory/lower-symlink
rename and shared-directory deletion, complete metadata and lower-hardlink
behavior, stable renamed-directory handles, external-update watchers, complete
private Git metadata ownership, and publication retirement. The mounted conflict
test does not use the activation helper or protect later edits during its
capture and conflict installation. File and directory sync alone do not close
those gates. Integrated helper costs appear above; this standalone fixture
excludes activation.

[git-conflict-results.json](git-conflict-results.json) records a standard Git
conflict worktree with 1,024 asset entries left unmaterialized. Initialization
uses `worktree add --no-checkout`, a sparse file selection, and
`read-tree --reset -u HEAD` to populate the new index and selected file before
merging. Ordinary `git merge` exits 1, writes diff3 markers, and exposes all
three versions through index stages 1/2/3. The agent edits the file, uses
`git add` and `git commit`, and gets a normal merge commit. A subsequent merge
after another shared update retains the resolution and that new update, ends
with clean Git status, and still materializes no assets. A later edit in the
primary workspace is unchanged. No custom conflict format or third-party
resolver is used. The separate existing 18 Git correctness cases also pass.

This conflict test runs native host Git on a trusted disposable fixture; it does
not execute the production activation script or the sparse filesystem. Its
single preparation/resolution timings are diagnostic component observations, not
activation benchmarks. It covers one text conflict; other conflict shapes and
actual activation recovery remain integration gates. The earlier native
publication medians of 16/26/146 ms for 100/1,000/10,000 files also remain
component timings outside the new filesystem and schema/hook path. There is no
integrated sparse-filesystem activation benchmark yet.

Verification at that revision passed the actual SDK build and all three focused
modes, matching recorded source hashes, Python syntax, Go formatting,
Markdown/JSON formatting, and whitespace checks. DOX verification passes 210
documents and 17 full roots, including exact child coverage, reciprocal parents,
links, and reachability. Analysis DOX and the implementation checklist are
updated. Broader development and root contracts remain unchanged because their
qualification requirements and production behavior have not changed.

## September 8 sparse Gofer transport qualification

The earlier custom Gofer transport revision passed 14 limited component checks
against 2 GiB of allocated random assets. Its source digests identify that
revision, before the namespace operations and focused write checks above. The
large-tree read benchmark has not been repeated; the complete workflow remains
unqualified. The current [probe](gofer_probe.go) retains the traversal mode:

```sh
python3 kernel/development/analysis/gofer_probe.py
```

The script builds a disposable executable from generated upstream SDK
`v0.0.0-20260815055033-7d8fb7f28de4`, with Go 1.26.5 and `CGO_ENABLED=0`.
Production runsc is unchanged. It creates 2,048 independently allocated random 1
MiB files, 10,000 180-byte source files, a native executable, and a tiny Git
fixture. Assets are outside that fixture's Git index. No full Git clone or
asset-history operation is timed. `WORKFLOW_GOFER_ASSETS=4` reduces the asset
fixture for diagnosis; the recorded final run uses the default 2 GiB.

The launcher creates a private user/mount namespace and a read-only shared
source bind inside a private storage root. The extension opens lower, upper, and
original directories from the prepared Gofer mount before its final chroot. It
serves only the merged view and donates regular-file descriptors. Directfs is
disabled for the sparse profile, with `overlayfs_stale_read` enabled for mapping
replacement during copy-up. This is a separate prototype mount layout, not
integration with the production development driver.

Initialization creates zero private source files. Replacing `Old label\n` with
`New label\n` creates just the original and edited file: 20 logical bytes and
8,192 allocated bytes, with zero asset copies. This count excludes directories,
runtime state, the shared fixture, and future Git/recovery metadata. The probe
also verifies:

- All 2,048 assets are available through native paths and directory enumeration.
- The original is captured before truncation and shared content stays unchanged.
- Fresh path reads and stat see shared atomic replacement while an old read
  descriptor remains open; that old descriptor retains its previous contents.
- Git's file-path and streamed hashes of the changed shared file agree.
- Native ELF execution, a read descriptor and shared read-only mmap opened
  before copy-up, and a subsequent shared writable mmap work. Shared source
  remains unchanged.
- Relative symlink access succeeds and a host-sibling symlink escape is denied.
- Completed private writes and originals survive runtime kill/delete/recreation.
- Unsupported native `git add` fails explicitly when creating `index.lock`. This
  expected failure confirms a missing operation; it does not qualify Git.

The two additional original/private mmap-test files bring the final private-file
allocation to 16,384 bytes. Completion of file copy-up and subsequent file sync
is tested; interruption during original/upper creation, host power loss, and
directory-sync guarantees are not tested or implemented. Other gaps are listed
explicitly in [gofer-results.json](gofer-results.json).

Each traversal is `find <tree> -type f -exec cat {} + >/dev/null` inside a real
sandbox, including one native exec. Each profile uses the same custom
executable; the stock profiles select upstream fsgofer without the extension,
with read-only native mounts. Debug logging is disabled for all timed profiles.
Profiles run sequentially over the same tree with unflushed host caches. First
means first traversal in that sandbox; warm is the median of the next three.

| Backend                   | 2 GiB assets, first | 2 GiB assets, warm | 10k small files, first | 10k small files, warm |
| ------------------------- | ------------------: | -----------------: | ---------------------: | --------------------: |
| Sparse Gofer probe        |             4.142 s |            2.921 s |                7.153 s |               6.740 s |
| Stock Gofer over RPC      |             2.562 s |            2.464 s |                4.954 s |               6.332 s |
| Stock Gofer with directfs |             1.953 s |            2.051 s |                4.197 s |               4.605 s |

Warm sparse traversal costs about 19% more than stock RPC for assets and 6% more
for small files, and about 42%/46% more than directfs respectively. These
samples have uncontrolled host scheduling and meaningful scatter; they do not
establish production overhead. The fixture is under the workspace-mounted host
filesystem, whose reported `statfs` type is `0x6a656a63`, not `/tmp`'s
`0x794c7630` OverlayFS used by earlier probes. Earlier absolute timings are not
comparable. Fixture generation, source caching, runtime creation, CPU/RSS,
multiple developers, Git capture, merge/validation, publication, and recovery
are outside these traversal timings. Full resource and correctness qualification
remains open.

Verification passes the actual executable build and component run, recorded
source digests, Python syntax, Go formatting, Markdown/JSON formatting, and
whitespace checks. The DOX audit passes 210 documents and 17 complete roots,
including child coverage, reciprocal links, and reachability. Analysis DOX and
the implementation checklist are current; broader DOX contracts are unchanged
because no production filesystem or publication behavior is adopted.

## September 8 small-edit and live-view requalification

The private-clone recommendation is withdrawn. Qualification remains active and
incomplete; a successful component experiment does not satisfy the full
developer workflow. The user accepts different mechanisms that keep shared
changes available during development, provide good conflict resolution and
durability, and avoid package-size copies for a small edit.

The new stock-overlay experiment uses a writable native user directory as its
upper layer, a read-only shared lower, and `userxattr`. It mounts inside the
disposable gVisor sandbox; no host FUSE or production mount change is involved.
Reproduce with:

```sh
python3 kernel/development/analysis/run.py upper
WORKFLOW_UPPER_DIRECTFS=false python3 kernel/development/analysis/run.py upper
```

Both commands currently exit 1. Each creates 1,024 random 64 KiB asset files (64
MiB allocated content) and edits one label. With an additional seven-byte
ignored artifact, the upper has two regular files totaling 17 logical bytes and
8,192 allocated bytes. No asset files are copied. Shared atomic replacement is
visible when no descriptor retains the old path; the private label and ignored
artifact survive kill/delete/recreate without a checkpoint.

Both modes fail the retained-descriptor case: after an old read descriptor is
opened and shared content is atomically replaced, a new path lookup still reads
the previous content. The old descriptor itself correctly retains its old data.
Both also panic in `overlay.filesystem.RenameAt` when `git status` replaces its
index and whiteout creation returns EPERM. Native host whiteout creation returns
EPERM as well. These failures are recorded in
[upper-results.json](upper-results.json) with commands and source hashes.
Restoring host whiteout permission would not establish live-path coherence,
correct originals, or publication safety.

These are storage and failure probes, not performance qualification. Upper-file
allocation excludes directories, originals, Git state, schema/hooks, and
publication. Final directfs variants ran concurrently in isolated fixtures, so
their sandbox startup timings are not a speed comparison. The fixture is 64 MiB,
not a measured multi-gigabyte production package.

## September 8 authorized Phase 2 qualification

The user authorized Phase 2 after the terminal phase. Experiments compare stock
runtime configuration and durable native Git storage in disposable roots.
[phase2-results.json](phase2-results.json) contains source/probe identifiers,
full logs, the 18-case Git matrix, raw benchmark samples, and package-size
observations. The recording-time kernel HEAD is
`2901eadfce244936efa4ec0adb7dce0873adc76d`. Concurrent unrelated changes were
preserved; the recorded mount-owner digest identifies the tested source. This
work changes analysis files and development documentation; it installs no
production filesystem or publisher.

Reproduce from the kernel root:

```sh
python3 kernel/development/analysis/run.py coherence current
python3 kernel/development/analysis/run.py coherence shared
python3 kernel/development/analysis/run.py native
python3 kernel/development/analysis/git_probe.py
python3 kernel/development/analysis/git_probe.py native-fresh
python3 kernel/development/analysis/git_probe.py native
```

The first command intentionally exits 1: seven of ten lower-file checks and the
path/stream hash comparison fail on production's exclusive lower cache. The
corrected stock profile passes all ten checks, equal hashing, private isolation,
and durable system/home storage. It uses `share=shared`, `--overlay2=all:self`,
and rootfs `source`/`type=bind` annotations with the overlay annotation omitted.
An initial `overlay=none` attempt was rejected as an unsupported medium; that
literal is not the tested configuration. Extra writable bind mounts are outside
this default-profile probe and would also receive overlays from the global flag.

Both stock profiles lose private source after abrupt runtime destruction. The
corrected profile also reveals mixed Git ownership: shared HEAD advances while a
private copied index remains at its former tree. Native status reports staged
reversions and unstaged shared updates. File revalidation does not supply
durable Git metadata or correct first-write merge bases.

The independent native Git mount passes:

- Executable loading; shared writable mmap followed by file sync; hardlinks and
  symlinks; atomic rename with an existing writable descriptor; independent
  flock exclusion; and local inotify create, move-to, and delete events.
- Publication from an immutable private commit while a real shell remains alive
  with the same PID, cwd, and open descriptor. A save after capture remains
  private; it neither changes the published candidate nor disappears.
- Native Git bundles streamed across the existing sandbox execution boundary in
  both directions. Workspace Git executes inside the sandbox; the host imports
  checked objects into its own repository and verifies the captured commit.
- Native conflict exit 3, diff3 markers, base/local/shared index stages,
  ordinary `git add`/`commit` resolution, and another merge after shared HEAD
  advances during resolution. The primary workspace's newer edits remain intact.
- A detached retained native PTY across both publications and conflict
  resolution, followed by reattachment to the same Bash PID.
- Abrupt kill/delete/recreation with no checkpoint: branch, commit, source,
  ignored data, and linked conflict worktree survive. Explicit archive copying
  to another user's disposable sandbox also survives removal of the original
  workspace.

The native probe deliberately confirms that untouched files do **not** follow
shared publication before explicit synchronization. It uses prototype Git
publication outside the production schema coordinator; it is not evidence that
the canonical activation helper is repaired. Host power failure, volume loss,
cross-node storage, external-write inotify, full runtime resource/concurrency
testing, and actual schema/hook recovery remain unqualified.

The new eighteenth Git case verifies successive native-branch publications:
retaining the private commit in published ancestry preserves the next merge
base; squashing it away creates a false conflict. Existing cases continue
covering disjoint and conflicting content, binary/mode/symlink changes, renames,
unusual filenames, mixed observed originals, ref CAS, and transfer.

First/warm traversal uses `find bench -type f -exec cat {} +` inside actual
sandboxes, with 10,000 ten-byte files. Timing includes one native exec per
sample. Warm is the median of three repeats; first means the first traversal in
that sandbox, not a flushed host page cache.

| Mount                     | First traversal | Warm traversal |
| ------------------------- | --------------: | -------------: |
| Current private overlay   |          720 ms |         617 ms |
| Corrected private overlay |          948 ms |         683 ms |
| Durable private Git mount |          692 ms |         598 ms |

Native Git timing below uses five trials per size with 171-byte tracked files,
one private content edit and a new ignore rule. Every trial changes the edited
content; validation refreshes do not repeatedly benchmark an identical tree.
Capture uses ordinary `git add`/`commit`, leaving subsequent writes in the
private working tree. Timing includes object transfer, merge, published commit
creation, stable validation checkout/refresh, and expected-ref/shared-worktree
publication. Fresh-validation trials additionally remove their checkout before
completion. The reused checkout is removed only at the end of the experiment,
timed separately.

|  Files | Initial clone, median | First publication and validation checkout | Repeat publication, median of four | Fresh validation each time, median of five |
| -----: | --------------------: | ----------------------------------------: | ---------------------------------: | -----------------------------------------: |
|    100 |               11.8 ms |                                   18.5 ms |                            16.0 ms |                                    20.3 ms |
|  1,000 |               33.6 ms |                                   59.5 ms |                            26.1 ms |                                    59.4 ms |
| 10,000 |              193.9 ms |                                  332.7 ms |                           145.9 ms |                                   367.4 ms |

The 10k private checkout uses 40.6 MiB allocated for working files and Git
state; the validation checkout requires additional storage. Actual first-party
HEADs have 727 files and 32.4 MB of content, mostly the demo package; this
excludes histories, ignored data, and block allocation. Full-history clone cost
grows with history. These fixtures test independent objects without alternates
and pass `git fsck` after deleting the source repository.

All timings use local LinuxKit/Docker OverlayFS and unflushed page caches, with
uncontrolled host scheduling. Fresh and reused variants run sequentially, not in
parallel; their scatter remains in the raw data. Git CPU and block-I/O deltas
cover each clone/reset/capture/publication trial, not just the reported
activation interval. Deno validation/hooks, Worker/HTTP overhead, production
coordination, recovery, and runtime RSS are excluded. An earlier
fresh-validation run measured 619 ms at 10k; the final paired runs above replace
it for comparison. No full-system latency or resource guarantee follows from the
component timings. Timed transfer uses trusted local fixture repositories. The
separate native sandbox test verifies complete fixture-history bundles through
the existing stream boundary; production stream admission, incremental transfer,
and that complete path's cost remain integration gates.

Host FUSE access was rechecked with a temporary device node: open returns EPERM.
The earlier external-FUSE mmap/ELF failure is at the pinned runtime client,
whose `ConfigureMMap` returns `ENOSYS`. No host FUSE filesystem or custom gofer
was qualified. The [report](REPORT.md) rejects the native explicit-sync
alternative for workspace adoption because of stale untouched files and
package-size copying. These measurements remain comparison evidence;
qualification of the required workflow is active and incomplete.

Phase 2 verification also passes Python syntax, Go formatting/compilation
through the native tests, Markdown/JSON formatting, and whitespace checks. The
DOX audit passes all 210 documents and 17 complete workspace/repository
frameworks, including relative links, reciprocal parents, exact child coverage,
and reachability. The development and analysis DOX record authorization and the
qualification outcome; broader contracts remain intact because no production
design is adopted.

## September 8 baseline refresh

Rechecked kernel `f4c6fe0d1af77d32a33f34e476a8cd51485e276f` with its existing
in-progress changes. This task changes analysis files only. Environment remains
LinuxKit 6.10.14, arm64, six CPUs, Go 1.26.5, Git 2.39.5, and pinned runsc
`release-20260817.0`. Only disposable repositories and sandboxes were used.
[current-results.json](current-results.json) retains the five probe logs and
complete current Git matrix/samples. The September 6 measurements below remain
historical; they are not evidence that the replacement filesystem shipped.

Commands run from the kernel root:

```sh
python3 kernel/development/analysis/run.py runtime
python3 kernel/development/analysis/run.py races
python3 kernel/development/analysis/run.py git
python3 kernel/development/analysis/run.py fuse
python3 kernel/development/analysis/git_probe.py
```

The runtime again overwrote another developer's disjoint changes, discarded
uncheckpointed source and ignored artifacts after runtime loss, removed the host
conflict worktree without exposing its annotations, and killed the helper caller
with exit 137 about 520 ms after invocation. The injected write after capture
was absent from both shared and private source. An unrelated repository reader
waited through the 200 ms schema hook and resumed after 231 ms.

The new native Git probe additionally proves:

- Native branch creation, staging, and committing do not alter shared source.
  Checkpoint/stop/start retains the edited file but loses the local branch and
  commit object, leaving the file uncommitted. Source patches are insufficient
  workspace persistence even during orderly shutdown.
- After `untouched.txt` changes from `before\n` to `shared-advance\n` in shared
  source, `cat` reads the complete 15 bytes but `stat` reports the former
  size 7. `git hash-object untouched.txt` hashes `shared-`; piping `cat` into
  `git hash-object --stdin` hashes the complete content. Native status is empty,
  and the activation preview incorrectly includes this untouched file. A
  successful ordinary read does not qualify live lower coherence for Git.
- The first probe ordering also failed activation preview with
  `failed to unpack
  tree object` after shared HEAD advanced. The final probe
  checks orderly checkpoint first and logs shared-update inspection afterward,
  so both persistence and metadata incoherence can be observed in one disposable
  run. A passing characterization is not a passing redesign acceptance gate.

All 17 existing Git merge/transfer/ref cases pass. Five-trial medians for one
small edited file are below. Candidate timing includes private index/tree/commit
construction and merge; the two-worktree comparison includes checkout, patch,
commit, and cherry-pick but excludes its separately measured cleanup.

| Tracked files | Two-worktree preparation | Cleanup | Per-path-original candidate |
| ------------: | -----------------------: | ------: | --------------------------: |
|           100 |                  15.7 ms |  2.5 ms |                      8.5 ms |
|         1,000 |                  68.6 ms | 12.1 ms |                     10.8 ms |
|        10,000 |                 466.2 ms | 96.3 ms |                     27.7 ms |

The three-trial ten-file, 20 MiB binary candidate median was 395 ms. These costs
exclude workspace capture, durable writes, validation/hooks, conflict
installation, and publication. Probes could overlap with independent checks;
host scheduling and page cache were uncontrolled. No end-to-end activation
latency guarantee follows from these measurements.

The repeated 1,000-file external FUSE probe still fails ELF execution and Git
index mmap. First/warm scans were 1,237/275 ms, versus 95/32 ms on the current
overlay. Warm is the median of three repeats. Host `/dev/fuse` remains absent;
host FUSE and custom gofer integration remain unqualified. The larger FUSE
measurements below were not repeated.

The actual `TestRootlessRetainedTerminals` passed again with two shell PIDs
through 20 detach/switch/attach cycles, 4 MiB detached output, and independent
close/exit. The existing `TestActivationPublishesOnlyAfterBothHooks` and
`TestActivationLockCoversDurableRecordThroughCompletion` also pass. Their latter
contract explicitly retains the shared deployment lock: the report now covers
the required package/deployment changes rather than suggesting that an
activation-only mutex replacement solves concurrency. Browser/htop evidence
remains in the [completed Phase 1 checklist](../WORKFLOW_IMPLEMENTATION.md).

DOX verification checked 210 documents and all 17 workspace/repository roots,
with framework text normalized for line wrapping. Hierarchy, reciprocal links,
reachability, and local report links pass. Go/Python syntax, whitespace, and
Markdown/JSON formatting pass. The analysis DOX records the new probe and
artifact; parent and runtime contracts remain unchanged because this refresh
adopts no production filesystem or publication behavior.

## September 6 initial investigation

Recorded 2026-09-06 against kernel source `ce6db1b`, with the local PTY fix,
test-fixture corrections, and opt-in experiments described below. No source
package was activated, and no developer sandbox was used. This records an
analysis, not completion of the proposed production redesign.

## Environment and reproduction

- Linux `6.10.14-linuxkit`, arm64, 6 CPUs; host Git `2.39.5`.
- Repository-local Go `1.26.5`; pinned runsc `release-20260817.0`.
- Real sandbox tests used rootless systrap and the installed development image.
- tmux `3.5a` was installed only inside a disposable test sandbox.
- Host `/dev/fuse` was unavailable; opening a temporary FUSE device node
  returned `Operation not permitted`. No host FUSE mount or custom gofer build
  was tested.
- Timing runs were sequential. Page caches were not flushed, and host scheduling
  was uncontrolled. These are local measurements, not service-level guarantees.

Run these from the kernel repository root after its normal portable runtime and
development-image installation:

```sh
python3 kernel/development/analysis/run.py runtime
python3 kernel/development/analysis/run.py races
python3 kernel/development/analysis/run.py fuse
WORKFLOW_PROBE_FILES=10000 python3 kernel/development/analysis/run.py fuse
python3 kernel/development/analysis/git_probe.py
```

`run.py` uses Go's temporary source-overlay facility to compile the three
`workflowanalysis` fixtures into the development package. It does not replace
production files. Each experiment owns temporary repositories, runtime records,
system roots, and sandbox cleanup. The runtime test installs tmux and requires
outbound package access; the Git, race, and FUSE probes can run offline after
the toolchain and development image are installed.

## Current runtime behavior

Raw output: [runtime-results.txt](runtime-results.txt).

| Observation                        | Recorded result                                                                                                                                                                                        |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Private edits and live lower reads | Shared `same.txt` stayed `base`; B read its private `B` while immediately seeing the host update to an untouched file.                                                                                 |
| Sequential A/B activation          | A succeeded in 315 ms, B in 340 ms. Final same-line content was B. A's non-overlapping first-line change was also erased by B. Both activations reported success.                                      |
| Sandbox lifetime                   | A's `/tmp` generation marker disappeared after activation. The persisted sandbox ID stayed the same.                                                                                                   |
| Genuine merge conflict             | Advancing shared HEAD after capture produced `conflicted` and `same.txt`. The host merge file had markers; cleanup deleted it while B retained unannotated private text.                               |
| Canonical helper conflict          | Returned exit 3, `success:false`, `status:conflicted`, and `conflicts:[same.txt]`. Its process generation survived. The fixture matches the production command's existing structured-failure contract. |
| Abrupt runtime loss                | An uncheckpointed new source file and an ignored artifact were both lost after killing/deleting runtime state and starting again.                                                                      |
| Canonical helper success           | Returned committed/pending-reset JSON, then the enclosing command exited 137 after about 530 ms. Its delayed post-activation marker was never written.                                                 |
| Direct console close after PTY fix | The waiting process died; before the fix, an idle read retained the PTY and left it unreachable but alive.                                                                                             |
| Two named tmux sessions            | Shell PIDs 126 and 128 survived 20 actual authenticated console WebSocket closes/reconnections and switches, with commands and screen contents verified. No abandoned tmux clients remained.           |
| Logout boundary                    | An authentication fixture rejected reattachment while logged out and allowed later reattachment; the tmux sessions survived. This was not a browser or real users-package logout test.                 |

The conflict was deliberately introduced between capture and preparation because
ordinary sequential activation currently chooses the new shared HEAD as its base
and silently overwrites instead. The runtime test exercises the real helper,
command-bus gateway, development manager, runsc, and console broker. Its command
registration and authentication are fixtures, not a booted full platform.

## Capture and lock ownership

Raw output: [race-results.txt](race-results.txt).

The deterministic fake driver wrote a file at entry to `Pause`, after activation
had captured its patch. Activation committed successfully, but neither the
published repository nor the restarted private view retained that late file.
This isolates the capture-before-pause race without relying on random timing.

While a schema hook deliberately waited for 200 ms, an unrelated reader of the
shared repository mutex could not proceed. It resumed after 221.557 ms when the
hook and activation finished. This demonstrates the broad lock's contention; it
does not prove an unavoidable deadlock or measure real schema-hook latency.

## Native Git correctness and cost

Raw samples and environment: [git-results.json](git-results.json).

All 17 cases passed: disjoint-line merge, same-line conflict, identical edits,
add/add, delete/modify, rename/edit, binary conflict, executable-mode/content
merge, symlink conflict, whitespace/newline paths, later first touch, mixed
per-path first-touch bases, mixed bases with shared renames, resolution followed
by another shared advance, stale compare-and-swap publication, plain-file
transfer, and transferred base objects preserving a rename.

Two intentionally incorrect base strategies were also rejected by assertions:
one old package HEAD falsely conflicts after a later first touch; a synthetic
base seeded from today's tree loses shared rename ancestry. Retaining the
original package tree and substituting each dirty path's observed original
passed both scenarios.

Each small-file benchmark has five trials and one edited 171-byte file. Git's
`merge-tree --write-tree -z` performs the merge; synthetic commits supply a
common parent on Git 2.39. The private-index candidate includes Git process
startup, index initialization, hashing, and tree/commit creation. The mixed-base
candidate includes a second index and synthetic base/current commits. The
two-worktree comparison includes both checkouts, patch application, a commit,
and cherry-pick; cleanup is measured separately. The merged trees must agree.

| Tracked files | Two-worktree preparation | Cleanup | Common-base candidate | Per-path-base candidate | Merge only |
| ------------: | -----------------------: | ------: | --------------------: | ----------------------: | ---------: |
|           100 |                  12.0 ms |  1.7 ms |                3.2 ms |                  6.6 ms |     0.6 ms |
|         1,000 |                  69.1 ms | 10.0 ms |                4.6 ms |                 10.0 ms |     0.7 ms |
|        10,000 |                 391.9 ms | 78.7 ms |               12.3 ms |                 23.1 ms |     0.7 ms |

The separate binary case changes ten distinct 2 MiB files, introducing fresh
blobs on each of three trials. Common-base candidate plus merge took 372 ms
median. The entire Git harness recorded 77,080 KiB maximum child-process RSS,
2.219 s total child user CPU, and 2.631 s total child system CPU. The RSS is a
child high-water mark across the harness, not total concurrent application
memory or filesystem-server memory.

These numbers exclude workspace snapshotting, filesystem generation bookkeeping,
conflict installation, schema validation, shared-source publication, and durable
commit acknowledgement. Index work still scales with tracked files. If schema
hooks require a materialized checkout, that additional full-tree cost must be
measured; the Git microbenchmark does not remove it.

The plain-file transfer test removes its original sandbox directory and merges
using saved originals in a new unrelated repository. That proves text recovery,
not arbitrary rename recovery. The second transfer includes a native base-only
Git bundle, removes the original repository too, and merges a shared rename.
That fixture's recorded bundle is 428 bytes; real export size depends on the
baseline objects needed, potentially including unchanged files.

## External FUSE prototype

Raw output: [fuse-1000-results.txt](fuse-1000-results.txt) and
[fuse-10000-results.txt](fuse-10000-results.txt).

The minimal Go FUSE server uses gVisor's external socket transport, without a
host FUSE mount. First write saves original and private bytes as regular host
files. Subsequent reads preserve the private version while untouched paths read
live lower changes. Removing a published private entry exposes the shared
version while the sandbox stays alive.

An ordinary ELF binary ran through the current package mount (exit 0) and failed
through external FUSE (`Permission denied`, exit 126). A Git index located on
external FUSE failed mmap (`Function not implemented`, exit 128). The scan
comparison therefore puts mutable Git indexes in normal sandbox `/tmp` storage
and measures source traversal, not the already-failed index operation.

|  Files | Mount                   |    First scan | Subsequent scans                     |  Warm median |
| -----: | ----------------------- | ------------: | ------------------------------------ | -----------: |
|  1,000 | Current private overlay |    122.703 ms | 95.514 / 93.361 / 118.060 ms         |    95.514 ms |
|  1,000 | External FUSE prototype |  1,709.243 ms | 335.637 / 332.222 / 329.860 ms       |   332.222 ms |
| 10,000 | Current private overlay |    544.908 ms | 146.704 / 146.232 / 148.174 ms       |   146.704 ms |
| 10,000 | External FUSE prototype | 15,121.828 ms | 3,387.727 / 3,587.070 / 3,644.456 ms | 3,587.070 ms |

The server is serial and deliberately minimal, with no data/attribute caching,
production crash journal, symlink/hardlink support, or correct generation-pinned
open-handle implementation. These timings reject this prototype as a general
development backend; they are not an upper bound on optimized FUSE throughput.
Neither host FUSE through the ordinary gofer nor the official custom gofer
extension has been implemented or benchmarked here.

## Supporting production fix and verification

The only production implementation change makes SCM_RIGHTS-imported console
descriptors nonblocking before `os.NewFile`. Go can then register them with its
poller and interrupt an idle read when the console closes. The regression
`TestReceivedConsoleCloseInterruptsIdleRead` failed before the change and passed
afterward. Existing console and SSH race tests also passed. Named-session
persistence itself remains a prototype.

Two fixture corrections accompany it: the SSH E2E expects the actual opaque
sandbox hostname, and activation command registration retains structured failure
results as the production handler already does.

Use the repository-local Go environment for existing checks:

```sh
export GOCACHE="$PWD/.development/cache/go-build"
export GOMODCACHE="$PWD/.development/cache/go-mod"
export GOWORK=off
.development/toolchains/go/bin/go test ./kernel/... -count=1
.development/toolchains/go/bin/go test ./kernel/sandbox/backend/runscconsole ./kernel/console ./kernel/ssh -race -count=1
THE8020_DEVELOPMENT_E2E=1 .development/toolchains/go/bin/go test ./kernel/development -run '^TestRootlessDevelopmentE2E$' -count=1 -v -timeout=10m
THE8020_DEVELOPMENT_OVERLAY_PROBE=1 .development/toolchains/go/bin/go test ./kernel/development -run '^TestRootlessDevelopmentOverlayProbe$' -count=1 -v
```

- Rootless development E2E passed in 22.448 s, including SSH, PTY resize/input,
  APT/dpkg persistence, helper activation, and reset behavior.
- The existing rootless overlay probe passed in 3.86 s.
- PTY/console/SSH race tests passed. The corrected runtime experiment passed in
  23.008 s; deterministic race and both FUSE characterization runs passed.
- The first full Go suite hit the unchanged
  `TestLoopbackManagerAllocatesDistinctPersistentControlPorts`: two allocations
  received inspector port 33257. A focused 20-run retry passed, then the full Go
  suite passed. The port-allocation issue was not changed or concealed.
- Go formatting, whitespace, and Python syntax checks passed. The DOX audit
  checked 195 documents and all 17 workspace/repository roots with no broken
  links, missing direct-child entries, parent mismatches, unreachable documents,
  or incomplete framework copies. Sandbox-parent, dev-core and UUI docs remain
  unchanged by this task where their ownership and implemented contracts did not
  change. Workspace and relevant kernel/development/PTY docs record the
  requested target, evidence, and actual fix separately.

Remaining production qualification includes the filesystem backend's POSIX and
crash behavior, concurrent saves during activation/conflict installation,
schema/source transaction recovery, actual browser navigation/login/logout,
rootful deployment, and control-plane restart adoption. No complete production
workflow, browser session selector, or restart-free activation is claimed.
