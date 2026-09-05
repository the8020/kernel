# Purpose

- Bootstrap every service/job Worker through one generic typed lifecycle.

# Ownership

- Own execution identity, dedicated MessagePort, structured log forwarding,
  cancellation, ES-module loading, service/job invocation, explicitly registered
  Worker control functions, lifecycle reporting, and uncaught-error reporting.
- Do not own application protocols, function schemas, or business behavior.

# Local Contracts

- Entrypoints load only here; Worker names include workload, owner, execution,
  and Worker identity.
- Workload types are exactly `service` and `job`. Worker permissions are
  explicit and no broader than the sandbox envelope.
- Jobs require a function default export and call it as
  `await module.default(...arguments)`. A named `run` export has no special
  meaning and no hidden context argument is appended.
- `hook_dispatch.ts` is an ordinary job entrypoint receiving ordered handler
  references, invocation scope, and mutable state. It freezes the scope and
  imports/awaits each handler's default export as `(state, scope)` in the same
  Worker, returning the final state. It creates no child jobs or Workers and
  carries no application indexing policy. The kernel resolves entrypoints and
  supplies the job release identity; a failure identifies its declaration and
  stops the chain.
- Service in-flight ownership lasts through complete response-stream consumption
  or cancellation so graceful stop cannot truncate a dispatch. `streams.ts` owns
  shared finish-once stream accounting for Worker and supervisor leases.
- A service Worker becomes idle after readiness and again only after its final
  in-flight request completes; new activity clears that timestamp. The
  supervisor reports the timestamp but does not decide when kernel policy should
  remove the Worker.
- Graceful shutdown waits on the same in-flight completion signal used by
  request accounting; do not add drain polling loops.
- `request_authentication.ts` invokes the composition-supplied package hook
  inside the existing Worker and ordinary bridge request scope before HTTP
  handlers or WebSocket acceptance. The generic runtime has no users-package
  path or SQL. Public requests omit this metadata and bypass imports,
  verification, and policy. Successful approval alone publishes
  `auth.authenticated`; streaming bodies and WebSocket callbacks restore that
  approved request context. Context getters do not perform I/O. Never introduce
  a second auth Worker, service, or sandbox.
- Request metadata carries the trusted effective user and current generic
  execution identity plus the kernel-observed client IP address and network
  scope, without cookies, route tokens, or application settings.
- Application code sends only the kernel operation and arguments. RuntimeWorker
  attaches immutable Worker execution identity and the current request ID;
  sandbox and workload identity come from the authenticated supervisor envelope
  rather than duplicated application fields.
- The kernel bridge uses `AsyncLocalStorage` to retain an exact request/job
  context for every asynchronous continuation. Each transport request uses a
  separate frozen context even when it belongs to the same persistent execution;
  cancellation and transaction scope therefore cannot cross requests. The bridge
  relays only declared generic kernel operations. The non-secret database
  backend is installed from trusted Worker metadata before module import;
  read-only database status is also available before execution, while all
  contextual kernel calls require an active request or job.
- `@the8020/context` reads that same frozen asynchronous context and exposes the
  validated user, outer service/job/program identity, and infrastructure IDs. It
  identifies the outer UUI service, not package-owned UUI session IDs.
- An entrypoint may export a validated `workerFunctions` map. Only those named
  functions receive bounded JSON input and generic execution context. Exact
  control may carry its supervisor-validated persistent-execution identity so
  lifecycle completion remains bound to that execution. Kernel calls from
  service controls retain the service's canonical ID rather than its internal
  pool/workload ID; arbitrary exports and `eval` are never callable.
- Job logs, execution context, and state reset between compatible reused
  invocations. Secure-input maps are execution-local and cleared in `finally`,
  including failures. Finalization closes the request/job database scope; Worker
  shutdown also requests prefix cleanup as a leak-safe fallback. A Worker with
  database access set to `none` never opens or closes a database scope, keeping
  schema evaluation independent of the database being initialized.
- Jobs and service requests share this execution-scoped database path; every job
  invocation has a distinct request ID and closes only its own scope.
- Structured command errors raised by the kernel SDK retain their code, message,
  and details across the Worker boundary; ordinary application failures remain
  bounded messages.

# Lifecycle

- Initialize once, load one compatible module/release, execute by workload
  policy, report terminal state, and close cleanly.

# Failure Behavior

- Import, contract, permission, cancellation, registered-function, and uncaught
  application failures become bounded structured errors without exposing
  supervisor credentials.

# Concurrency

- A job runs one invocation unless compatible reuse is explicit. A service uses
  strict single-concurrency or the supervisor's bounded balancing allowance;
  exact control calls are correlated and bounded.

# Public API

- `bootstrap.ts` is the Worker entrypoint; `contracts.ts` defines generic
  service/job and control-function contracts.

# Dependencies

- Deno Web Worker APIs, ES modules, transferable streams, and generated generic
  protocol types.

# Non-Responsibilities

- No sandbox lifecycle, placement, host ports, resource control, physical socket
  upgrade, or application routing decisions.

# Verification

- Worker tests cover default-export-only spread job invocation, secure-input
  isolation/cleanup, service/job network access, permission denial, nested
  Workers, denial of direct internal-token/socket access,
  fetch/streaming/WebSocket relay, registered controls, kernel-call
  cancellation, crash reporting, compatible reuse, and inspector names.

# Child DOX Index
