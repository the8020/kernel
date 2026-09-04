# Purpose

- Run one protocol-neutral infrastructure control plane inside each service/job
  sandbox and orchestrate its Web Workers.

# Ownership

- Own authenticated health/status/control endpoints, Worker registry, generic
  stateless/persistent service pools, job mappings, physical WebSocket relay,
  exact registered Worker control invocation, heartbeats, bounded events, drain,
  and shutdown.
- Do not own application messages, function names, state, routes, protocols, or
  business logic.

# Local Contracts

- One supervisor serves one runtime group and one workload type: `service` or
  `job`. Its bearer token never reaches Workers.
- Worker/job/service-pool/drain controls use generated versioned envelopes and
  validate message type, runtime-group identity, and correlation.
- Job control errors preserve bounded structured command failures while keeping
  ordinary runtime failures as plain messages.
- Service validation invokes pinned in-sandbox Deno with the configured
  cached-only/online dependency mode before readiness. Jobs may supply a bounded
  list of additional modules to type-check through the same validation path.
- Service pools are `stateless` or `persistent`, with hard bounded per-Worker
  in-flight capacity and a bounded queue.
- Persistent initial requests reserve an exact Worker binding; follow-ups reuse
  it, disconnect preserves it only for keep-alive, and an explicit generic
  completion releases it immediately. Cleanup is idempotent after authoritative
  kernel completion but rejects a binding owned by another Worker. Live
  WebSockets cannot be swept.
- Exact Worker control addresses one known Worker and invokes only a function
  explicitly registered by its entrypoint. An optional persistent-execution
  target must match that Worker's live binding in its version-specific service
  pool before invocation. Request and response JSON, errors, timeout,
  cancellation, and sizes are bounded; no Worker scan or arbitrary export
  invocation occurs.
- Physical WebSockets are relayed with bounded buffers. The supervisor never
  decodes the application's text or binary protocol.
- Status exposes bounded ready/failed Worker identity, load, and logs. A Worker
  crash remains isolated and unschedulable. Each Worker also reports its exact
  idle-since timestamp so the kernel can apply Worker keepalive with a
  deterministic clock.
- Every kernel callback carries the Worker's exact internal workload identity
  separately from any public service/request identity. The kernel-selected
  database backend is available synchronously before entrypoint import, while
  Worker policy either permits or denies database operations. Request completion
  and Worker shutdown close corresponding kernel transaction scopes.
- Kernel calls use HTTP/JSON over `KERNEL_SOCKET_PATH`. Each call opens a fresh
  Unix-socket connection so a restarted kernel can replace the socket without
  restarting the sandbox. Response reads complete at the declared HTTP body
  length and never depend on the sandbox transport propagating connection EOF.

# Lifecycle

- Construct → listen/register → healthy → drain → stop; each Worker follows
  explicit start/readiness/active/drain/stop/failure transitions.

# Failure Behavior

- Invalid authentication/protocol/identity is rejected. Capacity failure returns
  a service `503`; an unhandled supervisor failure exits for kernel detection.

# Concurrency

- Main-isolate maps are authoritative; scheduling, binding, completion, queues,
  and control invocation are serialized there. Concurrent duplicate start/stop
  lifecycle requests share one pending operation; a same-ID start with a
  different definition is rejected, and drain waits for pending starts.

# Public API

- `Supervisor` is the test API and `main.ts` is the image entrypoint.

# Dependencies

- Deno HTTP/Web Worker APIs, generic Worker bootstrap, and generated protocol.

# Verification

- Supervisor tests cover strict operation-specific kernel callback envelopes,
  authentication, lifecycle, stateless/persistent scheduling, exact reuse and
  completion, exact registered Worker invocation, streaming HTTP/WebSocket
  relay, metadata, independent session expiry and Worker idle time, concurrent
  idempotent lifecycle retries, logs, drain, bounds, cancellation, and crash
  isolation.

# Child DOX Index
