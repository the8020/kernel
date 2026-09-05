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
- Do not own password verification, cookies, authorization policy, application
  configuration, application function schemas, or service behavior.

# Local Contracts

- Public API is `kernel.crypto`, `kernel.admin.execute()`, `kernel.execution`,
  `kernel.secrets`, `kernel.packages`, `kernel.services`, `kernel.nodes`,
  `kernel.development`, `kernel.settings`, `kernel.events`, `kernel.programs`,
  `kernel.database.info()`, unified `kernel.database.execute()`,
  `kernel.database.transaction`, `kernel.database.tables`,
  `kernel.worker.invoke()`, and `kernel.execution.completePersistent()`.
- `kernel.events.emit(name, data)` returns an event ID and accepted listener
  count without waiting for their execution. Events are local to the emitting
  node and listeners inherit its user; kernel minute events use system identity.
- `kernel.programs.list()` returns ready runnable programs, including programs
  excluded from Home, with `uui` and `discoverable` booleans, package/commit,
  description, and entrypoint metadata. These flags do not change generic
  program execution.
  `kernel.programs.run({programId, arguments, username?,
  sandboxGroup?, timeoutMs?})`
  executes asynchronously and returns terminal state, result, logs, failure,
  execution ID, and package commit. Omitted user inherits the current caller;
  the ordinary execution owner validates it. `programs.ts` owns these generic
  models and the PackageEvent envelope. Schedules and history are owned by
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
  callback. Every kernel operation requires an active service request or job
  execution; evaluator Workers have database access disabled.
- `worker.invoke` requires exact node, sandbox, and Worker IDs plus a bounded
  function name and JSON input. It returns JSON or a structured generic error;
  it never knows which application registered the function.
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
- The bridge uses `AsyncLocalStorage` to retain the exact trusted
  service-request or job-execution context across asynchronous continuations.
  Every request or job gets a new frozen context containing its request ID,
  validated user, outer origin, cancellation signal, authentication metadata,
  and optional persistent execution ID. Worker metadata and transport input are
  copied into primitives before exposure, so caller mutation cannot alter the
  context or kernel-call identity. Concurrent transports in one persistent
  execution never mutate or reuse one context object. Completion closes the
  exact request database scope, and Worker shutdown closes its execution-scope
  prefix.
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

# Work Guidance

- Keep the public module independent of direct Deno filesystem/network
  permissions and keep application data opaque. Application Workers use their
  private MessagePort and never receive the supervisor token or Unix socket.

# Verification

- `kernel_test.ts` covers crypto/admin/execution-secret/private
  operation/database calls, exact Worker invocation, persistent completion,
  structured errors, interleaved and persistent request isolation, cancellation,
  unavailable calls, bounds, and bridge cleanup.

# Child DOX Index
