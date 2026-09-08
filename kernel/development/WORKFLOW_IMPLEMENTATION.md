# Development workflow implementation

The user requires two ordered phases. This checklist records implementation
status; the earlier [analysis](analysis/REPORT.md) and
[measurements](analysis/RESULTS.md) describe the existing failure baseline.
Preserve unrelated work in every independent source repository. Use only
disposable runtime instances for destructive lifecycle checks.

Current user instruction: stop after Phase 1 is finished and verified. Phase 2
must remain unstarted until the user revises its scope and authorizes
proceeding. The user also replaced Codex/Claude Code interface qualification
with htop.

## Phase 1 — persistent native terminals

Status: complete, verified, committed, and deployed to test5. The shared PTY
owner, package service, browser client, and named SSH attachment are
implemented. Native browser/OpenSSH/htop checks, cross-node attachment and
orphan cleanup, and direct/retained measurements pass. Work stops here for the
user's Phase 2 revision. The fresh test5 instance replaces test4 and serves HTTP
on port 80 and SSH on port 22. Authenticated browser login/session establishment
and native SSH execution pass; the welcome screen loads no terminal assets.

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
      result, then stop for the user's Phase 2 revision.

Phase 1 does not repair activation-driven sandbox recreation. Explicit sandbox,
kernel, or host destruction can end its processes; transport persistence is not
process checkpoint/restore.

## Phase 2 — isolated filesystem and Git qualification

Status: not started; awaiting the user's revision and authorization after
Phase 1. The checklist below is the prior proposal, subject to that revision. Do
not integrate speculative storage behavior into Phase 1.

- [ ] Establish representative full-path baselines and explicit correctness and
      performance criteria before evaluating candidates. Do not weaken failed
      gates.
- [ ] Qualify ordinary Linux executable loading, mmap, locks, atomic saves,
      symlinks/hardlinks, rename/delete, open descriptors, watching, and actual
      tools.
- [ ] Prove live shared reads for untouched paths, private edit isolation,
      per-path observed originals and rename ancestry, native Git
      merge/conflicts, accessible originals/both sides/annotations, and safe
      resolution/retry.
- [ ] Prove ordinary Git status/diff/staging/commits/checkout and private
      metadata without modified Git or caller-specific storage interfaces.
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
- [ ] Investigate credible alternatives after failures. Record assumptions
      separately from observed results.
- [ ] Integrate only a qualifying candidate, replace obsolete reset coherently,
      and verify the complete workflow including Phase 1 terminals across
      activation. Otherwise leave production filesystem/Git unchanged and report
      precise unresolved blockers with the completed Phase 1 retained.

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
12 package checks pass. Phase 2 remains on hold.

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
attachment and cleanup of that existing owner. Phase 2 experiments and
production filesystem/Git changes remain unstarted until the user's revision and
authorization.
