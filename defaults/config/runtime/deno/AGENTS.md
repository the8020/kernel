# Purpose

- Implement the package-neutral supervisor, Worker bootstrap, HTTP framework,
  and kernel SDK packaged in the service/job runtime image.

# Ownership

- Own authenticated supervisor control, service/job Worker lifecycle, generic
  stateless/persistent service pools, physical HTTP/WebSocket relay, structured
  logs, registered Worker control invocation, persistent completion, drain, and
  shutdown.
- Application modules own their service/job contracts, protocols, state,
  function names, and payload semantics. This tree owns no UUI behavior.

# Local Contracts

- The supervisor never imports application entrypoints; `worker/bootstrap.ts`
  loads each entrypoint in a named Web Worker with explicit permissions.
- Generic workload types are exactly `service` and `job`.
- Service bodies use transferable streams with cancellation and backpressure.
  Stateless slots count active HTTP streams or WebSockets; persistent slots
  count logical execution bindings and target one exact Worker through opaque
  execution identity.
- Application entrypoints may export a `workerFunctions` map. Only its validated
  named functions are invocable; input/output are bounded JSON, function names
  are never interpreted by generic code, and arbitrary module exports or `eval`
  are forbidden.
- `@the8020/kernel` exposes execution-scoped secure input, authentication/admin
  capabilities, typed private package/domain operations, a unified database
  bridge with scoped transactions, exact Worker invocation, and persistent
  completion. The Worker bridge binds calls to trusted current request/execution
  identity without cookies or route tokens.
- Current service metadata exposes package-neutral node, runtime-group, sandbox,
  Worker, and execution identity plus the kernel-observed client IP address and
  network scope. No application settings map is transported.
- Physical WebSockets are upgraded and relayed by the supervisor; application
  Workers own all logical message parsing and connection behavior.
- Logs and control request/response bodies are bounded and identity-associated;
  the Go job boundary scrubs declared secure values from output and failures.
- The bridge uses asynchronous execution-local context, so concurrent requests
  in one Worker keep independent kernel/database identity. Only credential-free
  database metadata is callable outside an active request/job.

# Lifecycle

- Supervisor listen/register → healthy → drain → stop. Worker
  start/readiness/active/drain/stop/failure transitions are explicit.

# Failure Behavior

- One Worker failure remains isolated while the supervisor is healthy;
  supervisor failure loses the sandbox's executions and is reported to the
  kernel. Invalid authentication, protocol, target identity, registration, or
  bounds fail explicitly.

# Concurrency

- Supervisor state mutates in its main isolate. Persistent binding, in-flight
  accounting, exact invocation, and completion are serialized there.

# Public API

- `supervisor/main.ts` is the image entrypoint. Exported supervisor, Worker,
  HTTP, and kernel modules are production SDKs or test APIs as documented by
  their child contracts.

# Dependencies

- Pinned Deno, Web Platform APIs, generated generic protocol models, and the
  bundled Hono/Zod HTTP framework; no Node, Bun, or `deno_core`.

# Non-Responsibilities

- No application protocol, package identity, sandbox lifecycle, host networking,
  cgroups, mount policy, placement, or business logic.

# Verification

- `deno task check` and `deno task test` cover service/job lifecycle,
  stateless/persistent scheduling, streaming, WebSockets, registered exact
  Worker invocation, persistent completion, permissions, cancellation, crash
  containment, and debugger names.

# Child DOX Index

- `http/AGENTS.md`: generic Hono/Zod HTTP and WebSocket service framework.
- `kernel/AGENTS.md`: package-neutral Worker-to-kernel SDK and bridge.
- `supervisor/AGENTS.md`: infrastructure control and Worker orchestration.
- `worker/AGENTS.md`: common bootstrap and service/job contracts.
