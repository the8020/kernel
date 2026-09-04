# Purpose

- Provide the package-neutral `@the8020/kernel` SDK and its Worker-to-supervisor
  bridge.

# Ownership

- Own typed current-user, login/logout, administrative command, system-database
  calls, execution-scoped secrets, private package/domain operations,
  exact-Worker invocation, and persistent-completion contracts; bind calls to
  trusted current request/execution context and correlate their results.
- Do not own password verification, cookies, authorization policy, application
  configuration, application function schemas, or service behavior.

# Local Contracts

- Public API is `kernel.auth.currentUser()`, `kernel.auth.login()`,
  `kernel.auth.logoutCurrent()`, `kernel.admin.execute()`, `kernel.execution`,
  `kernel.secrets`, `kernel.packages`, `kernel.services`, `kernel.nodes`,
  `kernel.development`, `kernel.settings`, `kernel.database.info()`, unified
  `kernel.database.execute()`, `kernel.database.transaction`,
  `kernel.database.tables`, `kernel.worker.invoke()`, and
  `kernel.execution.completePersistent()`.
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
  Transactions use opaque kernel-held tokens. Table administration uses the
  private runtime-operation bridge. The SDK never opens a database connection or
  receives credentials inside the sandbox.
- The kernel injects the non-secret SQLite/PostgreSQL backend into Worker
  metadata before module import so database query compilation needs no bootstrap
  callback. Every kernel operation requires an active service request or job
  execution; evaluator Workers have database access disabled.
- `worker.invoke` requires exact node, sandbox, and Worker IDs plus a bounded
  function name and JSON input. It returns JSON or a structured generic error;
  it never knows which application registered the function.
- `completePersistent` identifies the active logical persistent execution from
  trusted context and carries no application reason or semantics.
- Current-user reads trusted request context without returning authentication
  secrets. Passwords cross only the private Worker MessagePort and authenticated
  supervisor callback and never appear in diagnostics.
- Shared package-command argument helpers raise structured `invalid_arguments`
  failures; package command code uses the same error type for intentional
  not-found/conflict outcomes instead of flattening them into application
  exceptions.
- The bridge uses `AsyncLocalStorage` to retain the exact trusted
  service-request or job-execution context across asynchronous continuations.
  Every request or job gets a new frozen context containing its request ID,
  cancellation signal, authentication metadata, and optional persistent
  execution ID. Concurrent transports in one persistent execution never mutate
  or reuse one context object. Completion closes the exact request database
  scope, and Worker shutdown closes its execution-scope prefix.
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

- `kernel_test.ts` covers authentication/admin/execution-secret/private
  operation/database calls, exact Worker invocation, persistent completion,
  structured errors, interleaved and persistent request isolation, cancellation,
  unavailable calls, bounds, and bridge cleanup.

# Child DOX Index
