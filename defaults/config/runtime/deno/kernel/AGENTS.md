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
  `kernel.packages`, `kernel.database.query()`, `kernel.database.execute()`,
  `kernel.worker.invoke()`, and `kernel.execution.completePersistent()`.
- `kernel.secrets` provides typed list/get/set delegation. List and set return
  value-free summaries; get is deliberately explicit and returns the value to
  the authenticated administrative caller.
- `kernel.packages` provides typed index list/inspect/set, source inspection,
  version listing, repository inspect/pull/push/checkout, concise
  ID/commit/success synchronization results, and local creation by delegating to
  generic `admin.execute`; it adds no bridge operation and no package semantics
  to the supervisor.
- `kernel.database` sends bounded SQL statements and scalar parameters to the Go
  kernel and returns bounded ordered rows or affected-row counts. Portable SQL
  uses `$1`, `$2`, and so on; the SDK does not rewrite SQL. It never opens a
  database connection inside the sandbox.
- `worker.invoke` requires exact node, sandbox, and Worker IDs plus a bounded
  function name and JSON input. It returns JSON or a structured generic error;
  it never knows which application registered the function.
- `completePersistent` identifies the active logical persistent execution from
  trusted context and carries no application reason or semantics.
- Current-user reads trusted request context without returning authentication
  secrets. Passwords cross only the private Worker MessagePort and authenticated
  supervisor callback and never appear in diagnostics.
- The bridge retains trusted context for the service-handler Promise. A
  synchronous call uses its request; an asynchronous call is accepted only when
  one request is unambiguous.
- There is no application settings accessor or application-specific namespace.

# Work Guidance

- Keep the public module independent of direct Deno filesystem/network
  permissions and keep application data opaque.

# Verification

- `kernel_test.ts` covers authentication/admin/secret/package calls, exact
  Worker invocation, persistent completion, structured errors, request
  correlation, context ambiguity, unavailable calls, bounds, and bridge cleanup.

# Child DOX Index
