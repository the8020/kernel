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
- Service in-flight ownership lasts through complete response-stream consumption
  or cancellation so graceful stop cannot truncate a dispatch.
- A service Worker becomes idle after readiness and again only after its final
  in-flight request completes; new activity clears that timestamp. The
  supervisor reports the timestamp but does not decide when kernel policy should
  remove the Worker.
- Request metadata carries trusted authentication and current generic execution
  identity without cookies, route tokens, or application settings.
- The kernel bridge retains one request context for an HTTP stream or WebSocket
  lifetime and relays only declared generic kernel operations.
- An entrypoint may export a validated `workerFunctions` map. Only those named
  functions receive bounded JSON input and generic execution context; arbitrary
  exports and `eval` are never callable.
- Job logs and state reset between compatible reused invocations.

# Lifecycle

- Initialize once, load one compatible module/release, execute by workload
  policy, report terminal state, and close cleanly.

# Failure Behavior

- Import, contract, permission, cancellation, registered-function, and uncaught
  application failures become bounded structured errors without exposing
  supervisor credentials.

# Concurrency

- A job runs one invocation unless compatible reuse is explicit. A service
  honors supervisor-enforced stateless or persistent slot limits; exact control
  calls are correlated and bounded.

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

- Worker tests cover service/job imports, permission denial, nested Workers,
  fetch/streaming/WebSocket relay, registered controls, cancellation, crash
  reporting, compatible job reuse, and inspector names.

# Child DOX Index
