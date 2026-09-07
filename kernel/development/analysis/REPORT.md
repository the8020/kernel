# Development workflow analysis

Recommendation: give the development workspace its own durable filesystem
state, use native Git three-way merging for publication, and extend the shared
native console broker with persistent terminal sessions. Qualify screen recovery
using UUI's terminal engine. Activation must change source state while the
sandbox and its processes continue running.

This is an analysis and implementation recommendation. The complete redesign
has not been implemented. The experiments establish the failure causes, Git
semantics/costs, a tmux lifetime prototype, and a filesystem backend to reject. They do
not establish production performance for the recommended filesystem integration.

**Native tool compatibility is required**

The agent continues opening ordinary paths such as
`/workspace/packages/the8020/demo/main.ts`. Git, Codex, Claude Code, editors, and
build tools must use their normal filesystem operations. The filesystem owns
the choice of shared versus private storage and saves merge originals below
that interface. The agent needs no overlay API, content codec, or modified Git.
Executable loading, mmap, locks, atomic saves, links, open descriptors, and file
watching must behave correctly, including during publication. The proposed
filesystem has not yet passed that qualification; the external FUSE prototype
failed it and cannot be adopted as a general development filesystem.
Reliability and low system overhead are acceptance gates. Benchmark these native
operations under concurrent developer activity against the existing backend;
the Git preparation timings below do not establish filesystem performance.

**What fails today**

| Finding | Evidence |
|---|---|
| The later developer silently overwrites earlier work. | Two real gVisor sandboxes edited the same file. Both activations succeeded. B replaced A's same-line edit **and erased A's non-overlapping edit**. `scanPackageChanges` selects the shared HEAD at scan time, losing the original copy-up base. The existing `TestActivationRebasesPrivateOverlayOnCurrentSharedSource` even expects this overwrite. |
| Activation kills its caller. | The canonical `activate` helper returned success, then its enclosing shell exited **137** before writing its post-activation marker. `resetOverlayLocked` kills/deletes/restarts; the helper only postpones it by 300 ms. Keeping the `sbx-` ID does not preserve the process. |
| Conflict files are inaccessible to the agent. | A competing commit after capture did reach Git's conflict path. The helper returned exit **3**, structured `conflicted` status and paths, without restarting. The annotated file existed in the host merge worktree, then cleanup deleted it; the sandbox retained its original private text. |
| Writes during activation can disappear. | A deterministic driver-boundary injection wrote after capture, before pause. Activation succeeded; the file existed in neither shared nor private source afterward. Capture is not an atomic workspace snapshot. |
| Abrupt sandbox loss loses source. | New source and an ignored artifact disappeared after killing/deleting the runtime without checkpointing and starting again. The filestore alone cannot restore the overlay namespace. |
| Slow activation stalls unrelated repository work. | An unrelated repository reader remained blocked throughout an injected 200 ms schema hook; it resumed after 222 ms. The same repository mutex is shared with package administration and held across Git and hook I/O. This proves contention, not a universal deadlock claim. |
| Browser terminals have connection lifetime. | UUI disposes the socket on navigation; the console broker closes its PTY. Reconnect creates a new process. There is no named session to reattach. |

There was also a separate shared PTY bug: a blocking descriptor received from
runsc was not registered with Go's poller. Closing it left an idle read blocked,
retaining the PTY and sometimes an unreachable process. A small root-cause fix
now makes the descriptor nonblocking before `os.NewFile`. Its regression failed
before the fix and passes afterward, including race checks. The real experiment
changed from an orphaned sleeping process to proper transport cleanup. A stale
SSH test hostname expectation was corrected to use the actual sandbox ID.
The experiment's activation-command fixture was also aligned with production's
existing structured-failure contract; production already preserves that payload.

**The proposed developer flow**

1. Enter Development test. Start the user's existing sandbox if necessary and
   list its named terminals. Offer New, switch, rename, and explicit Close.
   Navigation, refresh, logout, and service replacement detach the view. They
   never stop the sandbox. Add no idle termination policy yet.
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
   state until explicit resolution; absence of marker
   text alone is not proof of resolution. After resolving, retry against the
   shared version actually presented, and merge again if another commit arrived.
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

| Version | Timeout near the top | Retries near the bottom |
|---|---:|---:|
| Original | 30 | 2 |
| A's published version | 60 | 2 |
| B's private version | 30 | 3 |
| Git's merged result | 60 | 3 |

The original tells Git that B did not ask to undo A's timeout change. If B also
changed that same timeout line to 45, Git would report a conflict. Activation
would return a failure with all sides available for resolution, without
restarting the agent. Publication is the subsequent commit/update of shared
source after a successful merge and validation.

The filesystem and publisher must share the same mutation contract. A watcher
cannot atomically distinguish a clean file from a write beginning just before a
host replacement. Merely retaining an old package HEAD also misattributes newer
untouched lower content as developer work. Neither repairs first-write ownership.

Retain a real package base tree from the start of its dirty generation, then
substitute each dirty path's observed original when constructing the synthetic
merge base. Seed it from that retained tree, not today's tree: the latter loses
shared rename ancestry and produces a false delete/modify conflict. Tests cover
both mixed first-touch versions and shared renames. Pin referenced Git objects
against garbage collection. Synthetic commits serve only the merge calculation;
the published commit descends from the actual shared HEAD.

**Durable workspace storage and locking**

Keep ordinary private files, saved originals, deletions/type/mode metadata, and
conflict records under a workspace directory beneath the user's existing durable
root. Its identity must be independent of a particular runsc sandbox. Keep mutable
Git indexes, refs, locks, and newly written objects private; only immutable shared
objects may be reused read-only. Activation consumes the workspace's dirty set,
not another developer's index or a full mounted-tree scan.
The live lower view has no single historical Git HEAD: ordinary `git status`
against a fixed private HEAD can include newer shared files. Filesystem
transparency alone does not settle that Git working-tree behavior. The helper's
dirty set may guide publication, but it cannot stand in for validating ordinary
`git status`, `diff`, `add`, `commit`, and `checkout` on the mounted workspace.
Do not introduce a Git wrapper or silently rewrite a developer's index to hide
the distinction; this part of the proposed Git experience remains unqualified.

Persist namespace changes transactionally at the mutation boundary. Save the
original before the first mutation; thereafter writes go directly to the durable
private file. Metadata can use a small transaction journal, with file/directory
fsync at its documented durability boundaries. No periodic scanner, full-tree
serialization, or per-write Git process is needed. Git ignore rules determine
publication, not whether a developer's file survives sandbox loss. Export is an
explicit copy/archive of private files, originals, and a readable manifest; import
can reconstruct text merges without the original sandbox. Full rename behavior
also requires the retained base tree/objects. Reuse the target's package objects
or include a base-only Git bundle during explicit export; that transfer can
include unchanged base content, but adds no background scanning. Tests proved
both plain-file text recovery and a transferred Git base preserving a shared
rename after the original sandbox/repository had been removed.

Use a bounded publication queue and short locks scoped to a workspace or selected
packages, in deterministic order. Perform candidate work and hooks outside broad
repository locks. Filesystem handlers must never wait for an activation job that
needs the same filesystem. Validate shared HEAD and workspace generation again
before publication; report contention or preserve later edits, rather than retry
indefinitely. Multi-package ref checks alone are not an atomic deployment: retain
the durable coordinator's recovery path.

Open writable descriptors, shared writable mmap, rename-over-save, hardlinks,
and crash ordering need explicit implementation tests. In particular, do not
delete an upper inode while a writer can still modify it. Pin writable generations
through handle lifetime and preserve writes after the captured snapshot. A
successful activation may retire only generations proven unchanged. This is
filesystem behavior, not a caller-side comparison immediately before `unlink`.

**Terminal choice**

Prefer persistent sessions at the shared native console boundary. The existing
[SSH adapter](../../ssh/server.go) already calls the same `OpenConsole` owner
used by the [browser relay](../../console/console.go). Both currently close that
lease when the client disconnects. There is no need to add another SSH hop:
retain the backend PTY and let either transport attach to its stable session ID.

The proposed split is:

- Kernel console owner: create/retain/close the PTY, keep reading its output,
  expose stable session identity, sequence output, and bound transport queues.
- SSH and WebSocket adapters: authenticate and attach/detach clients, forward
  native input/output and resize, and leave the persistent PTY open after a
  connection ends. A detached persistent session must receive neither implicit
  stdin EOF nor a terminal hangup; ordinary connection-bound SSH exec retains
  its existing EOF and exit behavior.
- Deno packages: own names, session selection, explicit close actions, and
  terminal display recovery independently of the temporary UUI screen or login
  session. Start with one active input/resize controller per terminal to avoid
  interleaved input and conflicting window sizes.

SSH channels belong to their connection. Reattachment is a new authenticated
connection selecting an existing application-owned terminal, not automatic SSH
channel resumption. The browser can continue using its existing WebSocket
transport to the same owner.
[SSH connection protocol](https://www.rfc-editor.org/rfc/rfc4254.html#section-6).

Process lifetime alone is insufficient: a fresh browser has lost the screen,
cursor, terminal modes, and scrollback, while the application may only emit
incremental updates. Keeping only the last output bytes cannot reconstruct all
that state; keeping and replaying the complete lifetime stream is unbounded.
Output must also be consumed while detached so an unread PTY cannot stall the
application once its buffer fills.

UUI already uses xterm.js. The project documents headless terminal state plus
its serialize addon for reconnection, making a matching headless engine in the
Deno-side terminal service a candidate. This retains one terminal capability
model across the browser and its saved state rather than introducing tmux's
separate model. Deno runtime compatibility and complete snapshot coverage remain
unverified; the serialization addon is documented as experimental.
[xterm.js headless use case](https://github.com/xtermjs/xterm.js#nodejs-support),
[serialize addon](https://github.com/xtermjs/xterm.js/tree/master/addons/addon-serialize).

Qualification must cover atomic snapshot-plus-output handoff, partial escape
sequences, alternate buffers, modes, Unicode, colors, links/graphics where
supported, and resize. Terminal queries need responses while detached, and
historical replay must not generate duplicate responses or repeat clipboard/
notification side effects. Existing transparent live byte forwarding is useful
evidence, but does not prove these reconnect behaviors.

Use bounded screen/history state and bounded sequenced output queues. Keep work
proportional to emitted output; do not poll idle terminals or retain their full
lifetime transcript in memory. Measure parser CPU, memory per retained terminal,
streaming throughput, reconnection latency, and slow-client behavior. No native
persistent-broker or screen-restoration benchmark has been run yet.

Authenticate every attachment. Logout must detach current views and prevent
unauthenticated reattachment, while the session remains available after login.
Do not use an authentication-session or UUI-session ID as terminal identity.
Explicit close and process exit end the terminal. No idle expiry is requested.

tmux remains a measured comparison, not a required part of this design. It
already owns terminal state, resizing, scrollback, and detached processes.
[tmux documentation](https://github.com/tmux/tmux/wiki/Getting-Started).

After the PTY fix, **20 real authenticated WebSocket closes/reconnections and
switches preserved both shell PIDs**, executed commands in both sessions,
preserved their displayed output, and left zero abandoned tmux clients. The
authentication fixture also rejected reattachment while logged out. This tests
the real broker/runsc/tmux boundary, not a completed browser selector or the full
users-package login/logout flow. The existing rootless development E2E also passes.

These were shell tests, not Codex or Claude Code rendering tests. tmux
interprets and redraws terminal output, so compatibility depends on the
application, tmux version/settings, and UUI's browser terminal. Anthropic
documents configuration for modified keys and notification/progress passthrough.
[Claude Code terminal configuration](https://code.claude.com/docs/en/terminal-config).
It also documents working fullscreen rendering with caveats: mouse mode is
needed for wheel scrolling, iTerm2's `tmux -CC` integration is incompatible, and
tmux through the 3.6 series lacks synchronized output and can add flicker. The
tested tmux 3.5a falls within that range.
[Claude Code fullscreen guidance](https://code.claude.com/docs/en/fullscreen#use-with-tmux).
Current tmux documentation includes synchronized-update support, so the older
version limitation must not be generalized to every version.
[tmux manual](https://man.openbsd.org/tmux.1#terminal-features).
Codex exposes `--no-alt-screen` to change its alternate-screen behavior; that
option does not establish overall tmux compatibility.
[OpenAI CLI reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).

Qualify the proposed native persistent path with both actual agents in the same
UUI terminal, including sustained streaming, resize, reconnect, input editing,
modified keys, paste, scrolling, selection, and supported graphics. Compare
against direct connection behavior and, where useful, the tmux prototype.
Record all versions. Persistent process IDs alone do not satisfy this gate.

Process survival covers connection changes and activation, not host reboot or
runsc failure; durable source must survive those failures. Current kernel startup
also deliberately destroys inherited development sandboxes. If “until shutdown”
includes control-plane restarts, development adoption and filesystem-owner
reconnection must replace that policy; process checkpointing is not implied.

**Measurements and filesystem selection**

LinuxKit 6.10.14, arm64, 6 CPUs, Go 1.26.5, Git 2.39.5, pinned runsc
`release-20260817.0`, rootless systrap. Temporary repositories reside under `/tmp`.
Git rows are medians of five trials, with one edited 171-byte file. Page cache was
not flushed. Candidate preparation includes both index initializations, original
and edited blob hashing, synthetic tree/commit creation, merge and process
startup; it excludes schema hooks and
source publication. It still processes index entries proportional to tracked
file count. The merge-only column excludes preparation.

| Tracked files | Two-worktree preparation | Its cleanup | Candidate with per-path originals + merge | Merge alone |
|---:|---:|---:|---:|---:|
| 100 | 12.0 ms | 1.7 ms | 6.6 ms | 0.6 ms |
| 1,000 | 69.1 ms | 10.0 ms | 10.0 ms | 0.7 ms |
| 10,000 | 391.9 ms | 78.7 ms | 23.1 ms | 0.7 ms |

Ten distinct edited 2 MiB binary files took 372 ms median for candidate creation
and merge against a common base (three trials with new blobs). Large content
still costs hashing and
storage; the design removes redundant tree materialization, not that work.
Seventeen Git cases passed, including same-line conflicts, disjoint auto-merges,
identical edits, rename/edit, delete/modify, add/add, binary/symlink conflicts,
executable modes, whitespace/newline filenames, resolution followed by another
publication, mixed first-touch bases with shared renames, stale ref rejection,
and portable originals/base objects.

| Filesystem option | Assessment |
|---|---|
| Existing private gVisor overlay | Cannot satisfy durable first-write state and safe in-place retirement through its current public integration. Filesystem checkpoint/restore remains an explicit snapshot operation, not continuous mutation persistence. |
| Ordinary private Git worktrees | Good Git isolation and normal files, but no transparent live lower view. Adding an asynchronous overwrite loop leaves a correctness race. |
| Linux OverlayFS with live lower edits | Reject: changing a mounted underlying layer has undefined behavior. [Linux contract](https://docs.kernel.org/filesystems/overlayfs.html#changes-to-underlying-filesystems). |
| External FUSE inside pinned gVisor | Reject for general development. The prototype proved first-write capture, private durable files, live lower reads, and retiring one upper without restarting. However, Git index mmap failed and an ELF executable failed. Scanning 10,000 small files cost **15.1 s cold / 3.59 s warm**, versus **0.545 s / 0.147 s** for the current private overlay. Warm is the median of three repeated scans. This is a minimal uncached prototype, not a ceiling on FUSE performance. |
| Host FUSE exposed through the ordinary gofer | Preferred first implementation to qualify: standard Linux filesystem semantics and unchanged upstream runsc. It needs host FUSE access and correct deployment/rootless mount handling. This environment denied opening a FUSE device, so its throughput, mmap and crash behavior were **not measured**. |
| Official custom gofer extension | Alternative if requiring host FUSE is unacceptable. The supported extension API is present in the inspected pinned-era source and supports custom LISAFS backends, but requires maintaining a custom runtime build. Preserve ordinary mmap/file-handle semantics and prevent directfs from bypassing copy-up. This integration was **not built or benchmarked**. [gVisor extension API](https://gvisor.dev/docs/user_guide/filesystem/#custom-gofer-extensions). |

The external FUSE path can run without host `/dev/fuse`, which made the
experiment possible; its availability does not establish full POSIX compatibility.
[gVisor FUSE transport](https://gvisor.dev/docs/user_guide/fuse/).

The storage/publication separation is the proposed direction; filesystem and
terminal selections remain conditional on qualification. Neither untested option
should be described as a measured performance winner. Before rollout, require:
two developers with concurrent saves/publications; full conflict-resolution retry;
process/cwd/TTY preservation during activation; mmap, executable, symlink,
hardlink, rename and directory behavior; crash injection at journal/publication
boundaries; export/import to a different sandbox; authentication logout and real
browser navigation/refresh; and scan/read/write latency on representative package
sizes. Source validation must consume a stable candidate through the shared
filesystem/mount owner; if a checkout is still materialized for schema hooks,
its full-tree cost remains outside the candidate benchmark above.

Implement in the user's required order: complete and verify persistent native
terminals first. Then qualify the durable filesystem and generation-safe Git
publication in isolated, disposable experiments. Integrate that second phase
only after its complete correctness, native-tool, and performance gate passes.
The implementation checklist is [WORKFLOW_IMPLEMENTATION.md](../WORKFLOW_IMPLEMENTATION.md).
Keep the PTY cleanup regression. Do not ship an intermediate activation mode that
just skips restart while retaining stale upper files, false merge bases, or lost
late writes.

Reproduction and raw observations are in [RESULTS.md](RESULTS.md),
[run.py](run.py), [runtime_test.go](runtime_test.go),
[races_test.go](races_test.go), [fuse_test.go](fuse_test.go), and
[git_probe.py](git_probe.py). The experiment code uses disposable source overlays
and does not install a new production filesystem or session feature.
