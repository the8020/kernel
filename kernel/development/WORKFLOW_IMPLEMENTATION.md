# Development workflow implementation

The user requires two ordered phases. This checklist records implementation
status; the earlier [analysis](analysis/REPORT.md) and
[measurements](analysis/RESULTS.md) describe the existing failure baseline.
Preserve unrelated work in every independent source repository. Use only
disposable runtime instances for destructive lifecycle checks.

Current user instruction: Phase 2 is authorized; find the best solution through
disposable qualification. Immediate shared updates on untouched files remain the
working requirement. The user replaced Codex/Claude Code interface qualification
with htop for the completed terminal phase.

## Phase 1 — persistent native terminals

Status: complete, verified, committed, and deployed to test5. The shared PTY
owner, package service, browser client, and named SSH attachment are
implemented. Native browser/OpenSSH/htop checks, cross-node attachment and
orphan cleanup, and direct/retained measurements pass. The fresh test5 instance
replaces test4 and serves HTTP on port 80 and SSH on port 22. Authenticated
browser login/session establishment and native SSH execution pass; the welcome
screen loads no terminal assets.

- [x] Extend the shared kernel console owner with stable PTY identity and
      separate create, attach, detach, process-exit, and explicit-close
      lifetimes. Browser and SSH attachments use this owner without tmux,
      screen, or another SSH hop. Keep connection-bound SSH exec, EOF, signals,
      environment, and status.
- [x] Give each terminal one input/resize controller. Detached output continues
      draining; history and transport memory are bounded; a slow view cannot
      block the owner. Kernel-owned idle expiry follows the last user detach.
- [x] Apply kernel-owned 36-hour detached-terminal and two-hour empty-sandbox
      defaults. Reattachment cancels expiry; retained terminals protect the
      sandbox until destruction. Verify with seconds-long settings, including
      ordinary SSH, metadata completion, and checkpoint restoration.
- [x] Deno owns names, selection, and terminal display state independently of
      browser, UUI, and authentication sessions. Development provides multiple
      terminals with create/list/select/rename/close.
- [x] Qualify matching headless xterm and serialization: atomic snapshot and
      subsequent-output ordering, cursor/modes, both buffers, partial sequences,
      UTF-8, Unicode, colors, supported graphics/links, and resize. Detached
      queries receive one response; replay repeats neither replies nor
      clipboard/notification side effects. Test omitted serializer state
      explicitly.
- [x] Verify real browser navigation, reload, switching, logout/login, network
      loss, detached output, reattachment, explicit close, and process exit.
- [x] Verify unmodified htop in the browser, including rendering, shortcuts,
      scrolling, resize, and recovery after disconnects. Keep the separate
      terminal input/editing/paste/selection checks; Codex/Claude interface
      tests are waived by the user.
- [x] Record baseline and retained-terminal CPU, RSS, throughput, slow-client
      behavior, and reconnect latency with versions, workloads, and sample
      counts.
- [x] Run affected checks and update DOX. Record a complete reviewable Phase 1
      result.

Phase 1 does not repair activation-driven sandbox recreation. Explicit sandbox,
kernel, or host destruction can end its processes; transport persistence is not
process checkpoint/restore.

## Phase 2 — isolated filesystem and Git qualification

Prototype delivery: complete in the separate opt-in build. Ordinary package
creation/deletion, annotated UUI conflict editing, native terminal handoff, and
continuation pass through the compiled kernel and real browser. The native
schema check also verifies catalog retirement, retained table data, and
recreation of a deleted package. [Prototype instructions](analysis/PROTOTYPE.md)
own build and use; [verification](analysis/prototype-results.json) records the
checks.

Ordinary symlink deletion/rename and UUI Git side selection also pass the native
activation fixture. The added namespace check still fails reading a retained
symlink descriptor after unlink. This exceptional compatibility case is deferred
with broader qualification; no current package requires tracked symlinks.

Broader Phase 2 qualification and production adoption remain deferred, as the
user requested. The private-clone recommendation is withdrawn: it leaves
untouched files stale and copies a package for a small edit. The prototype uses
the sparse Gofer and existing native Git, with no asset checkout for a label
edit. Installed instances retain their previous filesystem behavior. See
[Phase 2 results](analysis/RESULTS.md) and the
[recommendation](analysis/REPORT.md).

The custom Gofer transport probe now demonstrates a small existing-file edit
without asset copies, retained-descriptor freshness, native executable/mmap
behavior, and persistence of completed edits/originals through runtime
recreation. Its 2 GiB fixture is measured. Namespace support now passes atomic
file replacement/deletion, private links, local inotify, native Git commits, and
text-conflict resolution with durable markers/index stages. Shared regular-file
renames now preserve originals and open source/replaced-target handles through
private writes and recreation; no-op and type errors leave no private state.
Directory rename, retained symlink descriptors, complete metadata and hardlink
semantics, crash recovery, private Git ownership, activation/publication
integration, and live retirement remain unqualified; this is component progress
only.

Nested deletion now leaves parent directories and untouched siblings visible,
including shared sibling updates and helper publication/recreation. Private-only
directory removal uses native `rmdir` and rejects nonempty directories. An SDK
protocol fix makes a removed directory handle return no entries even when its
old path is recreated. Shared-directory removal now persists independent
directory tombstones, preserving child originals and hiding later shared
additions across recreation. Activation now captures and acknowledges ordinary
directory removal, resolves child-file conflicts through native Git, preserves
upstream additions and later directory operations, and restores live shared
paths. Native checks cover lost replies and recreated retry. Whole-package
lifecycle now passes; joint directory-mutation crash recovery remains open.

Filesystem-owned capture/acknowledgement now preserves later atomic, descriptor,
mmap, and mode edits, retires quiescent unchanged private files, and keeps the
correct original for the next merge. Duplicate/stale acknowledgements,
continuing process/cwd/descriptor behavior, and completed-checkpoint recreation
pass in a 77-check fixture. The publication profile disables the package dentry
cache; its measured small-read cost and earlier-revision write timings are
recorded separately. Automatic retirement after held files close and interrupted
transaction recovery remain open; the later integration fixtures below
distinguish their activation/schema/hook coverage.

The real development driver now runs the sparse mount with private native Git
metadata and read-only shared object alternates, without a source checkout.
Standard `.git` reference files point outside the package tree; native
rename/removal no longer encounter per-package Git mount points. Removed
references remain absent across recreation while private history and conflict
worktrees survive. Whole-package lifecycle is covered below. Unresolved/resolved
Git worktrees, branches, originals, and later edits survive driver recreation.
Native resolution/retry, incremental transfer, and a retained PTY across
test-driven publication pass. The existing helper's authenticated preview also
passes with low object-write cost after a disposable scanner fix. This
integration exposed and fixed parent-directory identity loss in the Gofer and
qualifies a one-line SDK error-propagation patch with a failing unpatched
control. The legacy production helper still removes its conflict worktrees.
Borrowed objects now use read-only retained hardlinks; native checks preserve
history and unresolved conflicts through shared GC, repository removal and
runtime recreation. Replacement-repository fetch and packed-object links pass.
Reclamation, repeated-repack storage, host-crash durability and broader private
Git semantics remain open. See
[integration results](analysis/sparse-runtime-results.json).

The disposable activation result now carries errors that occur before package
results exist. Saved state, the command gateway and native helper retain the
reason. Human CLI formatting and UUI conflict editing now share those native
worktrees; the real browser/terminal handoff passes.

The separate disposable activation owner now passes the actual helper's native
conflict/resolution/retry/publication loop. It retains a durable attempt and
per-path captures, preserves later edits and the terminal, and sends a complete
native candidate to a checking schema hook. Unchanged assets are hardlinked;
visible lower aliases survive private writes and recreation.
Deletion/recreation, schema rejection, and shared updates during validation have
focused checks. A private shared-file rename also merges and publishes an
upstream edit to the original path. This is the real helper/HTTP/CBus path with
a replacement publication owner, but not the actual schema engine or production
coordinator. Broader concurrent recovery, native namespace gaps and complete
costs remain open; the further schema fixture below covers selected two-package
recovery boundaries. See
[helper results](analysis/sparse-activation-results.json).

A further disposable check now joins the actual Deno table evaluator, SQLite
schema engine, native job/Worker runtime, and package activation coordinator. It
rejects an incompatible default change without publishing or losing private
work, accepts an ordinary Git candidate correction, adds a nullable column while
preserving data, and executes both activation hooks. After an injected error
between database completion and private-file acknowledgement, recreated
coordinators resume the same attempt without repeating successful hooks. The
extended sequence interrupts before/after preparation and before/after the last
of two native package resets. Recovery retains later edits, refuses dirty shared
trees and reconciles the exact saved transaction with source/catalog state.
Mid-reset dirty trees, full process/host crashes, PostgreSQL concurrency, and
transport/authentication outside the fixture remain open. See
[schema results](analysis/sparse-schema-results.json).

An additional regression found that retrying an already completed activation
could finalize a different prepared transaction before its source switched. A
disposable shared-contract patch now binds package/schema preparation and
completion to one activation ID across all existing callers. Existing owner
checks and the real helper/schema interleaving pass. The native attempt retains
that ID. Failed rollback now stays pending until cleanup succeeds. A durable
preparation phase lets retry settle the exact old preparation before starting a
new transaction for the retained native candidate. This repairs those recovery
boundaries in the prototype; full interrupted-reset and concurrent source-owner
recovery remain open. See
[transaction results](analysis/transaction-results.json).

The shared database groundwork now admits pending deployments for disjoint
package sets and completes only the selected ID's changes against the latest
catalog. Interleaved publication, overlap rejection, pending visibility, and
removal/rollback isolation pass. The coordinator/evaluator prototype now permits
unrelated activations during a paused hook or table evaluation. Exact-ID
operation admission rejects simultaneous duplicate requests; durable package
claims reject overlap between calls. A short metadata lock bounds admission to
256 unfinished activations and never spans ordinary evaluation or hooks. Focused
race checks and native schema/helper recovery checks pass. A further regression
found recovery rolling back a live checkout between preparation and switching.
The shared package lock now uses stable native files outside package
directories; development activation, administration and recovery hold the same
selected package ownership through completion. Tests verify unrelated checkout
progress, cross-process exclusion, directory replacement, partial acquisition
cleanup and release after owner death. Startup handling of live pending
activations, runtime indexing, PostgreSQL and shared-storage deployment remain
qualification gates; concurrent activation as a whole is not yet qualified.

A further regression found a failed index refresh already marked complete. The
prototype now retains a durable `published` stage until runtime indexing and
source-backup cleanup finish. Recreated completion and recovery retry that work
without repeating schema work, hooks or package revisions, and refuse rollback
or overlapping activation. The native helper check preserves an edit made after
the failure and the live sandbox while publishing the handler index. The copied
packages table definition includes the new stage; production remains unchanged.
Recovery now visits all unfinished attempts in a bounded snapshot even when an
older attempt is busy. The startup overlay checks activation records after
schema completion and reads source commits after recovery; its full initial
index includes the final recovered set. Native SQLite/evaluator checks preserve
one live preparation while recovering two abandoned attempts, then recover the
released owner. Starting a node during live publication and complete runtime
convergence remain open.

Published-revision regressions now pair the package commits and revision in one
database snapshot. Preparing a next activation retains the last published
commit; startup observes the baseline before its full index pass. The unchanged
poll remains one scalar query. Native schema coverage follows an actual
publication while the same package's next activation prepares. This does not
qualify the remaining startup ready-state/source comparison or complete
concurrent indexing.

Preparation also retains existing published availability. The prototype removes
the shared step that hid an already ready package from programs, selectors and
other packages' handlers/hooks. Unfinished activation status stays in its owning
record; first activation still waits for publication. The native schema fixture
runs an ordinary program job while the next activation of its package is
prepared. Source remains mutable and exact-source schema checks remain intact.

Runtime indexing now claims selected packages without holding a global lock
across queries, provider jobs or runtime actions. A newer refresh remains
pending after older work succeeds. Independent revision-consumer calls use the
same pending owner, and late acknowledgements cannot consume a newer
observation. Package/app race checks cover the actual SQLite index follower and
paused provider calls. Periodic polling now queues provider work through that
same indexer, allowing unrelated updates and database health checks to proceed.
At most 16 background batches run with ordinary job deadlines; overflow remains
pending, and shutdown cancels/joins owned work. Explicit activation/reindex
keeps its synchronous path. The actual monitor loop is checked with manual
ticks, SQLite revisions and provider/health doubles. Native Deno monitor
integration, Git-comparison locking, and live startup remain qualification
gates. Native handler and command discovery now read outside publication locks
and reject stale snapshots using an in-memory counter. Owner regressions cover
cross-package reference updates, independent and same-package command updates,
and a full rebuild finishing last. Disabling only the counter check reproduces
lost newer declarations. Sustained contention can repeat selected inspection;
its throughput is not yet measured.

The user accepts the measured read overhead for the prototype and prioritizes
writes and the ordinary edit/activate/Git-conflict/resolve/retry loop. Synced
existing-file write timings and a native sparse Git conflict worktree now have
component evidence. Atomic saves now also have synced first/repeated timings;
full activation qualification remains open. Conflict handling must use standard
Git files/markers/index stages and commands without a third-party resolver or
custom protocol.

Five single-label helper activations now have measured latency, storage and
process-resource counters with 4 MiB and 2 GiB of tracked assets. Medians are
1.13 s and 8.65 s with a checking hook, excluding the actual schema engine. No
asset contents are copied, but host child-process reads reach gigabytes per
activation. A native Git control isolates ctime invalidation from creating and
removing validation hardlinks. Validation I/O and accumulated attempt/index
cleanup remain open. [Results](analysis/RESULTS.md) retain the exact boundaries;
this does not close the full cost or activation gates below.

The activation owner now releases acknowledged capture payloads after syncing
their identity receipts. Native checks preserve unresolved captures, reject ID
reuse and continue a published attempt after a lost release reply and runtime
recreation. Later edits and next originals survive. Receipt retention, native
worktree/index cleanup and joint power-loss recovery remain open.

- [ ] Establish representative full-path baselines and explicit correctness and
      performance criteria before evaluating candidates. Do not weaken failed
      gates.
- [x] Qualify ordinary Linux executable loading, mmap, locks, atomic saves,
      symlinks/hardlinks, rename/delete, open descriptors, watching, and actual
      tools in the durable native Git alternative. Local inotify passes;
      external-write notification and live-overlay retirement remain
      unqualified.
- [ ] Prove live shared reads for untouched paths, private edit isolation,
      per-path observed originals and rename ancestry, native Git
      merge/conflicts, accessible originals/both sides/annotations, and safe
      resolution/retry.
- [x] Prove ordinary Git status/diff/staging/commits/checkout and private
      metadata without modified Git or caller-specific storage interfaces in the
      native alternative, including conflict resolution/retry and archive
      transfer.
- [ ] Prove mutation-owned durability of source and ignored artifacts without
      scans/autosave/full-tree copies, crash recovery, readable export/import,
      and retained originals/base objects.
- [ ] Prove publication preserves processes/cwd/open handles and concurrent or
      unselected edits without sandbox pause/restart. Conflict installation
      cannot overwrite later edits. Preserve schema/source coordination,
      activation metadata, failure recovery, and concurrency/idempotency
      guarantees.
- [ ] Measure full capture/persistence/validation/publication/recovery CPU, RSS,
      I/O, latency, and throughput for cold/warm trees, small and large files,
      multiple developers, and repeated activation. Bound locking and retries.
- [x] Investigate credible alternatives after failures. Record assumptions
      separately from observed results.
- [ ] Integrate only a qualifying candidate, replace obsolete reset coherently,
      and verify the complete workflow including Phase 1 terminals across
      activation. Otherwise leave production filesystem/Git unchanged and report
      precise unresolved blockers with the completed Phase 1 retained. Leaving
      production unchanged is required while qualification continues; it does
      not establish that the overall goal is achieved.
- [x] Deliver the prototype's human UUI conflict screen using the existing code
      editor, a conflicting-file selector, labelled and colored conflict
      sections, and continuation once all files are resolved. Verify
      UUI-to-terminal and terminal-to-UUI resolution/retry against the same
      retained native Git attempt. Make helper output readable with file paths
      and concrete next steps; do not introduce another merge state.

## Evidence

The shared console broker owns retained PTYs and stable identities. Detaching a
browser or SSH connection releases its input/resize lease without EOF or process
recreation. One controller owns input and geometry; the canonical Deno processor
continues consuming output and answering queries while clients are detached.
History, input, recovery allocations, and browser/native output queues are
bounded. `terminal.idle_timeout` defaults to 36 hours after the last user
attachment leaves; the canonical processor and output do not reset it.
`development.idle_timeout` then allows two hours after the last ordinary console
or retained terminal closes before checkpointing and stopping the sandbox.

Named SSH access uses
`ssh -tt -p <port> <user>@<owning-node> the8020 terminal-id <session-id>` with
optional `sandbox-id=<sbx-id>`. A PTY is required. Session names contain 1–40
ASCII letters, digits, `_`, or `-` and connect or create within the selected
sandbox. Labels survive physical expiry; reopening creates a new physical ID.
Ordinary SSH exec, environment, stdin EOF, signals, and exit status retain their
connection-bound behavior. SSH receives a Deno-rendered view of the same
canonical state, without replaying raw queries or historical clipboard effects.
Kernel transport does not interpret terminal VT.

The native browser/OpenSSH/htop fixture passed after the final parser batching
change: terminal `tty-9iqws0nj1o`, Bash PID 3, and htop PID 79. Two SSH
attachments, function/search/scroll keys, resize, navigation, reload, network
loss, actual logout/login, exactly one query response, browser control takeover,
process exit, and independent explicit close passed with no browser exceptions.
The rendered htop screen was inspected. The separate deterministic Chromium
fixture covers modified keys, Escape, Unicode bracketed paste and input bounds,
scroll/selection, snapshot continuation, and control transfer. Logs are
`/tmp/8020-terminal-native-batched-ssh.log` and
`/tmp/8020-terminal-batched-browser.log` in the disposable test environment.

The final two-node fixture passed with freshly built binaries and runtime:
terminal `tty-n8p9vnh6db`, Bash PID 3. HTTP and WebSocket attachment through the
second node reached the original owner. Killing its exact display Worker made
that signed route return 409 while the physical Bash process stayed alive.
Explicit close from the second node then destroyed the PTY and removed its
metadata. This exposed and verified a necessary native exact-node close path:
the owning kernel can clean up a terminal without a surviving display Worker. It
uses the existing authenticated recipient transport with bounded control and
matching acknowledgements, without node fallback or retry. The Deno service
retains metadata if native close fails. See
`/tmp/8020-terminal-native-resilience-remote-close.log`.

Twenty-one terminal engine/owner/recovery/service tests and 19 generic SDK
bridge tests pass. The earlier complete Go suite passed; the final native close
change also passes focused node, operation, console, callback, and app tests,
plus the node/operation/console/callback race checks. Package checks and the
affected DOX format/link checks pass. The native harness stages disposable
package copies and nodes; configured recipient listeners are restarted before
terminal creation. No real developer sandbox was used for lifecycle destruction.

The 0.4.1 idle-lifecycle fixture passed with a rebuilt kernel and disposable
runtime using eight-second terminal and two-second sandbox deadlines. Browser
and repeated named SSH attachment preserved Bash PID 3; continuing detached
output did not prevent expiry. Physical closure removed package metadata, then
the sandbox stopped after its separate deadline. Ordinary SSH restarted it,
restored a private package file, kept it running while connected, and triggered
idle stop after disconnect. See `/tmp/8020-idle-native-verified.log`. The full
Go suite, focused lifecycle race tests, all 105 generic-runtime tests, and all
12 package checks pass.

Matching xterm 5.5.0 headless and browser engines use the package-owned state
component. Stock framebuffer serialization omitted parser continuation, partial
UTF-8/escape sequences, saved cursor, margins, tabs, and character-set state.
The production continuation probe passes 121 headless and 242 Chromium cases;
engine regressions additionally verify batched output, query responses, and
resize ordering. Native display tests cover both buffers, history, RGB, styled
Unicode, links, input modes, and stalled-view isolation. Earlier probe and
broker-only measurements remain in [analysis/](analysis/AGENTS.md); they are
component evidence, separate from the full-system results below.

The package's [performance report](../../../dev-core/terminals/PERFORMANCE.md)
records exact versions, workloads, sample counts, and raw data. For three
two-MiB payloads at 80×24, median retained throughput was 11.8 MiB/s versus 40.7
MiB/s direct. Retained recovery took 0.195 seconds while direct reopen started a
new process in 2.04 seconds. CPU for two MiB was 0.36 versus 0.05 seconds. One
coarse eight-console sample measured marginal retained RSS/PSS of 32.8/9.1 MiB
versus direct 25.6/2.3 MiB; shared runtime GC and background cleanup limit the
precision of those deltas. The stalled browser detached, the producer completed,
and the same terminal delivered another exact two-MiB payload. Measurements
exclude canvas painting. Submitting bounded native reads together to xterm's
ordinary parser queue improved retained throughput from approximately 1.1 to
11.8 MiB/s; the SDK also uses native Uint8Array Base64 conversion.

Terminal browser code, dependencies, styling, and content-hashed assets belong
to `dev-core/terminals/frontend/` and `dev-core/public/`. UUI contains the
generic custom-element wrapper and loads program assets only when requested. UUI
owns asset versioning, caching, and ETags; the Deno supervisor's native HTTP
listener is the sole automatic compressor. Existing encoded responses pass
through once. Fifteen encoding/revalidation cases plus HEAD, stale versions, and
HTML passed through the actual Worker/listener/Go client path. Its measured
shell was 308,362 bytes raw, 155,380 gzip, and 102,915 Brotli. These are
delivery-path measurements, not a repeated throttled-login timing. The terminal
close and performance changes add no asset compressor.

Activation still recreates the development sandbox and ends its running
processes. Kernel/sandbox/host destruction is outside retained-transport
survival. Display-Worker loss is reported explicitly; it does not recreate the
terminal or claim that canonical display state survived. New terminal creation
runs on the service node that owns the requested sandbox; cross-node tests cover
attachment and cleanup of that existing owner. Phase 2 experiments are now
authorized; production adoption remains subject to the qualification gates
above.
