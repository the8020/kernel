Parent DOX: [kernel/defaults/config/runtime/deno DOX](../AGENTS.md).

# Purpose

- Provide the package-neutral `@the8020/kernel` SDK and its Worker-to-supervisor
  bridge.

# Ownership

- Own typed signing/verification, administrative command, system-database calls,
  execution-scoped secrets, private package/domain operations, exact-Worker
  invocation, and persistent-completion contracts; bind calls to trusted current
  request/execution context and correlate their results.
- Own the private execution-local provider consumed by `@the8020/context`;
  public context types and getters remain in the sibling context package.
- The bootstrap's internal executionContext accessor reads that same ALS store
  for log capture and execution-local redaction. It is not a public SDK API and
  never forwards the secure-input map to the supervisor or logger.
- Do not own password verification, cookies, authorization policy, application
  configuration, application function schemas, or service behavior.

# Local Contracts

- `kernel.services.restart(serviceId, mode = "soft")` publishes a generic
  restart across existing node placements. `hard` also terminates draining
  generations. Both return observed local status and preserve disabled policy;
  source update orchestration stays with the package/update owner.

- `newId` and `isId` expose the shared operational ID contract to package
  creation owners; they perform no kernel call.
- Public API is `kernel.crypto`, `kernel.admin.execute()`, `kernel.execution`,
  `kernel.secrets`, `kernel.packages`, `kernel.services`, `kernel.nodes`,
  `kernel.development`, `kernel.terminals`, `kernel.settings`, `kernel.events`,
  `kernel.programs`, `kernel.database.info()`, unified
  `kernel.database.execute()`, `kernel.database.transaction`,
  `kernel.database.tables`, `kernel.worker.invoke()`, and
  `kernel.execution.runPersistent()`/`completePersistent()`.
- `terminals.ts` exposes native PTY
  open/create/list/inspect/attach/read/write/respond/resize/detach/close through
  typed private operations. Creation atomically attaches the canonical output
  processor. Binary data uses base64 only on the JSON bridge; the package API
  uses Uint8Array. Read acknowledges the last applied output sequence; write
  awaits native consumption of one bounded frame before the next. Uncertain
  writes must not be replayed. Attachments belong to the exact calling Worker,
  while PTYs have independent broker lifetime.
- `terminals.open` accepts a sandbox-scoped `sessionId` of 1–40 ASCII letters,
  digits, `_`, or `-`, shell options, and the current persistent owner
  descriptor. It returns the live processor owner or a new attachment with
  `after` and `reset`. A reset preserves a surviving shell with fresh display
  state; recreation after process loss assigns a new internal physical ID.
- Use the pinned runtime's native Uint8Array Base64 conversion on terminal
  frames; do not allocate a JavaScript string iterator and temporary number
  collection for each output byte.
- Canonical `terminals.respond` acknowledges bounded queue admission, so the
  interpreter can keep consuming output while the process is not yet reading
  stdin. Ordinary input acknowledgement still waits for native consumption.
- `nextView` waits for an existing terminal's native display transport;
  `writeView` sends one bounded frame and `finishView` ends that stream. Only
  the exact canonical processor Worker may use them. Packages own rendering,
  recovery allocation, slow-view limits, and asynchronous send queues. Control
  attachment raises `TerminalControlBusyError` when occupied; `take-control`
  explicitly revokes the prior browser or SSH lease.
- Physical destruction raises `TerminalClosedError`, including on pending
  processor reads and native-view waits. It is distinct from process EOF or a
  lost attachment. Package owners use it to complete their handler; metadata
  retention belongs to the package and terminal idle policy to the kernel.
- `terminals.detach` remains callable during cancelled-request cleanup, as does
  database scope cleanup. Input, resize, and process destruction retain normal
  cancellation. Worker death releases all of that Worker's attachment roles.
- `terminals.close(terminalId, signal?)` closes locally;
  `terminals.close({terminalId, nodeId}, signal?)` addresses the exact physical
  node. Close is idempotent for an absent terminal on a reachable owner and
  requires no display Worker. An unavailable node fails without alternate-node
  routing or retry.
- `kernel.logs.query(query, signal?)` returns one bounded page using the same
  snake-case filters and opaque positions/cursors as `kernel.logs`. Optional
  node_id selects the exact owner; omission selects this node. Log records
  preserve source/component, typed identities, usernames and readable stacks.
  `formatLogRecord` renders their compact identifying header and multiline text.
- `kernel.logs.follow(query, {signal, intervalMs?})` yields one page at a time,
  defaults to a recent tail, and polls at 500 ms after reaching the current end.
  Empty pages may advance a scan; expired/unavailable pages end following.
  Retain only the current page/cursor and one outstanding call. Explicit signal
  cancellation stops polling and cancels only that kernel call, independently of
  other work in the same execution. The execution's own signal still applies.
- `kernel.events.emit(name, data)` returns an event ID and accepted listener
  count without waiting for their execution. Events are local to the emitting
  node and listeners inherit its user; kernel minute events use system identity.
- `kernel.programs.list()` returns ready runnable programs, including programs
  excluded from Home, with `uui` and `discoverable` booleans, package/commit,
  description, and entrypoint metadata. These flags do not change generic
  program execution.
  `kernel.programs.run({programId, arguments, username?,
  sandboxGroup?, timeoutMs?})`
  executes asynchronously and returns terminal state, result, failure, package
  commit, allocated node/sandbox/Worker/job/context IDs, saved log position, and
  invocation times. Allocated references survive execution failure; results
  contain no log messages. Omitted user inherits the current caller; the
  ordinary execution owner validates it. `programs.ts` owns these generic models
  and the PackageEvent envelope. Schedules and history are owned by
  `/p/the8020/jobs/mod.ts`.
- `kernel.execution.secret()` reads one required value from only the active job;
  `optionalSecret()` returns `undefined` when absent. No service or concurrent
  job can observe it.
- `kernel.secrets` provides typed list/get/set delegation. List and set return
  value-free summaries; get is deliberately explicit.
- `kernel.packages` provides typed index list/inspect/set, source inspection,
  version listing, repository inspect/pull/push/checkout, concise
  ID/commit/success synchronization results, and local creation through one
  generic private runtime-operation bridge; the supervisor interprets no package
  semantics.
- `kernel.database` sends compiled SQL and explicitly tagged values to the Go
  kernel and returns ordered rows, affected counts, and optional insert IDs.
  Transactions use opaque kernel-held tokens. Optional `timeoutMs` bounds
  acquisition and total lifetime; `lockTimeoutMs` bounds engine lock waits.
  Table administration uses the private runtime-operation bridge. The SDK never
  opens a database connection or receives credentials inside the sandbox.
- The kernel injects the non-secret SQLite/PostgreSQL backend into Worker
  metadata before module import so database query compilation needs no bootstrap
  callback. Every kernel operation requires an active service request, retained
  handler, or job execution; evaluator Workers have database access disabled.
- `worker.invoke` requires exact node, sandbox, and Worker IDs plus a bounded
  function name and JSON input. It returns JSON or a structured generic error;
  it never knows which application registered the function.
- `services.route(target)` reconstructs a signed descriptor from canonical
  node/sandbox/Worker/persistent-execution references. It creates no binding;
  later admission still verifies live service membership and the principal.
- `execution.runPersistent(handler)` claims one handler for a zero-keepalive
  service binding before invoking application work. It inherits the validated
  identity, uses Worker lifetime instead of the HTTP signal, and owns a separate
  `ctx-` database scope whose parent is the establishing request. Finishing the
  handler closes that scope and completes the binding. Lost establishment
  responses cannot discard a claimed handler; abrupt Worker release cleans its
  remaining scopes and native attachments.
- `completePersistent` identifies the active logical persistent execution from
  trusted context and carries no application reason or semantics. The existing
  Worker MessagePort resolves it in the owning supervisor; it sends no Go RPC.
- `kernel.crypto.sign/verify` transport arbitrary bytes as base64;
  `kernel.crypto.token.sign/verify` use the fixed platform JWT profile. Claims
  belong to Deno callers. Private keys never cross the bridge. All trusted
  services and jobs may sign; no signing permissions or allowlists exist.
- Shared package-command argument helpers raise structured `invalid_arguments`
  failures; package command code uses the same error type for intentional
  not-found/conflict outcomes instead of flattening them into application
  exceptions.
- `AdminCommandError.execution` retains an allocated command's log reference,
  including its job/node/context IDs, saved position and time range. Local
  validation failures have no execution reference. This metadata contains no
  copied console messages and creates no additional execution identity.
- The bridge uses `AsyncLocalStorage` to retain the exact trusted
  service-request or job-execution context across asynchronous continuations.
  Every request or job gets a new frozen context containing its `contextId`,
  optional parent context and job-run ID, validated user, outer origin,
  cancellation signal, authentication metadata, and optional persistent
  execution ID. Worker metadata and transport input are copied into primitives
  before exposure, so caller mutation cannot alter the context or kernel-call
  identity. Concurrent transports in one persistent execution never mutate or
  reuse one context object. Completion closes the exact request database scope,
  and Worker shutdown closes its Worker-scope prefix.
- Worker metadata and each request/execution must contain a canonical user.
  Missing identity fails before invoking application code; the bridge has no
  hardcoded user fallback.
- Cancelling an execution removes only its pending calls, sends correlated
  cancellation through the trusted supervisor, and reaches the Go callback
  request context. Late results for cancelled calls are harmless.
- The read-only `database.info` call may run from identified Worker module
  initialization; every SQL, transaction, authentication, administration, and
  typed operation call still requires an active execution context.
- There is no application settings accessor or application-specific namespace.

- `ServiceIndexScope.packages` is the immutable selected package/commit set.
  `ServiceIndexState.packages` holds mutable per-package service arrays and
  optional explicit errors. Hooks share the complete state in one invocation;
  the Go owner validates scope and publishes each accepted fragment.

# Work Guidance

- Add SDK surface only for a necessary shared kernel capability. Keep
  application validation and orchestration in their owning packages; evolve the
  typed bridge and its owner together, with bounds, cancellation, identity
  isolation, and affected service/job coverage.

- Keep the public module independent of direct Deno filesystem/network
  permissions and keep application data opaque. Application Workers use their
  private MessagePort and never receive the supervisor token or Unix socket.

# Verification

- `kernel_test.ts` covers crypto/admin/execution-secret/private
  operation/database calls, exact Worker invocation, persistent completion,
  retained-handler lifetime and separate scope cleanup, structured errors,
  interleaved and persistent request isolation, cancellation, unavailable calls,
  bounds, and bridge cleanup.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
