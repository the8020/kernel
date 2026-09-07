Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Broker lifecycle-tracked interactive processes in running 80|20 sandboxes and
  bridge the authenticated local browser WebSocket transport to them.

# Ownership

- Own target-provider selection, transport-neutral active process leases,
  retained physical PTYs with stable identity, bounds, provider-withdrawal
  cleanup, plus the generic console WebSocket protocol,
  same-origin/authentication gate, byte relay, resize relay, and connection
  cleanup.
- Do not own sandbox lifecycle, authorization policy, terminal rendering,
  command presentation, or backend-specific PTY creation.

# Local Contracts

- `/_the8020/console` is loopback-only through the main kernel listener and
  requires a valid platform JWT and users-package session approval, plus the
  `the8020.console.v1` subprotocol.
- JWT failure is rejected before an ordinary users-package validation job
  starts. Selected invalid cookies are cleared with the common platform cookie
  scope.
- Any authenticated user may currently open a console in any selected running
  runtime or development sandbox. Granular authorization is deferred to the full
  permission system.
- `OpenConsole` is the shared kernel transport boundary used by WebSocket and
  SSH. Every returned PTY or direct process stream is registered until close,
  and broker/provider shutdown closes it regardless of transport.
- `ResolveTarget` resolves canonical `sbx-` IDs against providers' current
  `HasSandbox` ownership. Unknown and conflicting owners fail closed; a prefix
  never distinguishes development from runtime, and process creation is never a
  lookup probe.
- Broker leases preserve an attached backend process's real exit status for
  transports such as SSH and preserve its separate stderr stream.
- `CreateTerminal` is the retained-PTY Go API. Its `tty-` identity and physical
  process belong to the broker, independently of creating request contexts. Deno
  accesses it through typed terminal operations; `dev-core` owns its display
  engine and retained browser/SSH display adapters. The existing `OpenConsole`
  wire path remains connection-bound.
- `OpenTerminalView` binds a native transport to an existing terminal and its
  exclusive controller. Its opaque display pipe is supplied by the sole
  processor through `NextView`/`WriteView`/`FinishView`. Each write is at most
  64 KiB and acknowledges transport consumption. The package must send outside
  its canonical parser queue and bound slow views. Missing processor ownership,
  mismatched sandbox IDs, and stale view IDs fail explicitly.
- `TakeControl` atomically revokes the prior input/resize lease across browser
  and SSH transports. View cancellation or processor loss interrupts blocked
  display I/O without ending the physical process. Native EOF on a retained
  transport detaches only; raw PTY bytes are never used as recovered display.
- A retained terminal permits four attachments with one input/resize controller.
  Detach releases only the attachment, never stdin EOF or the PTY. Accepted
  input frames drain in order across controller handoff through one writer;
  input is bounded to sixteen 64-KiB frames plus one in-flight frame. Full
  queues reject input explicitly and cannot block broker shutdown.
- `WriteFrame` waits for native consumption and lets transports send bounded
  paste frames without queue-admission retries. Cancellation cannot retract
  accepted bytes. Input failures release queued acknowledgement waiters while
  output retains ownership of final process bytes.
- Read output continuously, including while detached. Retain at most one MiB or
  1,024 sequenced output/resize events and return at most 256 KiB of data per
  read. Reads wait on owner events, never an idle poll. A lagging consumer gets
  an explicit gap error and needs a Deno display snapshot; the byte tail alone
  is not screen recovery. Returned events cannot mutate retained data.
- `CreateTerminalWithProcessor` attaches the sole canonical Deno interpreter
  before reading startup output. Its applied-sequence acknowledgement bounds
  unprocessed bytes without dropping them. Only this interpreter can send query
  responses. Browser reads never acknowledge its credit. Processor detach
  releases backpressure and keeps the PTY draining; missing interpreter history
  is an explicit gap, never claimed as recovered screen state.
- Process EOF retains bounded final output until explicit or idle terminal
  close. Exit status is optional because the native detached runsc PTY exposes
  EOF but no status. Explicit close unregisters the identity; provider
  replacement or broker close terminates its PTYs. There is no cross-kernel,
  sandbox-destruction, or host-reboot process restoration.
- Node-local `terminal.idle_timeout` defaults to 36 hours. The last user
  attachment's departure starts its deadline; reattachment cancels it and the
  next detach starts a full interval. The processor, output, and heartbeat
  traffic do not count. A new terminal without user attachments is already idle.
  Runtime changes recalculate existing deadlines from the original detach time.
  Expiry claims destruction under the attachment lock and uses ordinary close.
- Development console admission reserves sandbox lifetime before provider I/O.
  Failed opens release it; ordinary consoles release on close, retained
  terminals only on physical destruction. Process EOF and detached retained
  views do not release that reservation. The development manager owns sandbox
  shutdown.
- Direct leases, retained terminals, and pending process openings share the
  32-console bound. Reserve capacity before provider I/O and cancel pending
  openings on broker shutdown.
- The first frame selects a bounded target, direct argument vector, environment,
  working directory, and terminal size. Later binary frames are input; text
  frames are resize controls; server binary frames are output.
- Console sessions expose no password, SSH key, sandbox token, host port, or
  port-forwarding capability.

# Work Guidance

- Keep the broker transport-only and use provider/backend interfaces for all
  target, stream, and PTY behavior.
- Keep byte sequencing and attachment ownership here; Deno packages own names,
  display recovery, and workflow. Do not treat the retained-byte recovery window
  as a framebuffer or suppress ordinary connection-bound SSH EOF.

# Verification

- Unit tests cover authentication, same-origin/subprotocol enforcement, target
  selection, transport-neutral leases, binary relay, resize, malformed/bounded
  frames, provider removal, and close cleanup. Real rootless/browser/SSH tests
  prove Bash PTY behavior.
- Retained-owner tests cover creation-context separation, native-opening bounds,
  exclusive concurrent control, stale-attachment rejection, output/resize order,
  detached draining, explicit gap errors, input bounds, final output, provider
  replacement, and close with blocked I/O. Run console and SSH tests with
  `-race` after shared lifecycle changes.
- Native display tests cover exclusive processor access, canonical display
  bytes, controller takeover, slow-writer cancellation, processor loss, and
  detach without EOF or physical destruction.
- Short-timeout tests cover processor/output exclusion, observers, reattachment,
  live deadline changes, and pending-open lifetime release. Run with `-race`.
- `THE8020_TERMINAL_E2E=1 go test ./kernel/development -run
  '^TestRootlessRetainedTerminals$'`
  from the repository root exercises real gVisor PTYs: two shells, twenty
  detach/reattach/switch cycles, four MiB of detached output, explicit close,
  and independent process exit. This is not browser display or agent
  compatibility qualification.
- `go test ./kernel/console -run '^$' -bench
  'Benchmark(ConsoleOutput|TerminalRead)$' -benchmem`
  measures broker-only throughput/allocation using 16-KiB native-stream fixture
  frames. It excludes runsc, Deno, transport framing, and browser rendering.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
