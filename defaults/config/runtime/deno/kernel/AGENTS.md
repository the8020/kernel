# Purpose

- Provide the package-neutral `@the8020/kernel` SDK and its Worker-to-supervisor
  bridge.

# Ownership

- Own typed current-user, bootstrap login/logout, administrative command,
  system-database calls, secret/package-management convenience methods,
  exact-Worker invocation, and persistent-completion contracts; bind calls to
  trusted current request/execution context and correlate their results.
- Do not own password verification, cookies, authorization policy, application
  configuration, application function schemas, or service behavior.

# Local Contracts

- Public API is `kernel.auth.currentUser()`, `kernel.auth.bootstrapLogin()`,
  `kernel.auth.logoutCurrent()`, `kernel.admin.execute()`, `kernel.secrets`,
  `kernel.packages`, `kernel.database.info()`, unified
  `kernel.database.execute()`, `kernel.database.transaction`,
  `kernel.database.tables`, `kernel.worker.invoke()`, and
  `kernel.execution.completePersistent()`.
- `kernel.secrets` provides typed list/get/set delegation. List and set return
  value-free summaries; get is deliberately explicit and returns the value to
  the authenticated administrative caller.
- `kernel.packages` provides typed index list/inspect/set, source inspection,
  version listing, repository inspect/pull/push/checkout, concise
  ID/commit/success synchronization results, and local creation by delegating to
  generic `admin.execute`; it adds no bridge operation and no package semantics
  to the supervisor.
- `kernel.database` sends compiled SQL and explicitly tagged values to the Go
  kernel and returns ordered rows, affected counts, and optional insert IDs.
  Transactions use opaque kernel-held tokens. Table administration delegates to
  the existing command bus. The SDK never opens a database connection or
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
- The bridge uses `AsyncLocalStorage` to retain the exact trusted
  service-request or job-execution context across asynchronous continuations.
  Concurrent calls in one Worker never guess between requests. A persistent
  execution keeps one context object whose request identity is updated when its
  current transport reconnects, so its suspended program continues through the
  active registered request. Exact Worker control may borrow that context but
  never replaces its current transport identity. Completion asks the kernel to
  close the exact database scope, and Worker shutdown closes its scope prefix.
- The read-only `database.info` call may run from identified Worker module
  initialization; every SQL, transaction, authentication, and administration
  call still requires an active execution context.
- There is no application settings accessor or application-specific namespace.

# Work Guidance

- Keep the public module independent of direct Deno filesystem/network
  permissions and keep application data opaque.

# Verification

- `kernel_test.ts` covers authentication/admin/secret/package/database calls,
  exact Worker invocation, persistent completion, structured errors, interleaved
  asynchronous request correlation, unavailable calls, bounds, and bridge
  cleanup.

# Child DOX Index
