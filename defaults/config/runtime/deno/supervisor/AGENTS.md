# Purpose

- Run one protocol-neutral infrastructure control plane inside each service/job
  sandbox and orchestrate its Web Workers.

# Ownership

- Own authenticated health/status/control endpoints, Worker registry, generic
  stateless/persistent service pools, job mappings, physical WebSocket relay,
  exact registered Worker control invocation, authoritative runtime snapshots,
  bounded events, drain, and shutdown.
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
- Service pools are `stateless` or `persistent`, with a bounded queue.
  Concurrency one is strict; larger concurrency values are balancing targets
  with exactly one temporary extra slot per Worker and never unbounded overload.
  Main-isolate pending admissions close the handoff between Worker selection and
  the Worker's synchronous in-flight increment, so concurrent dispatch cannot
  claim the same strict slot.
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
- The main isolate synchronously records Worker starting/ready/stopping/stopped/
  failed transitions, request activity, persistent reservations, idle time, and
  recent failures. Status exposes bounded identity, load, and logs; callback
  snapshots omit logs, are absolute and revisioned, and remain observed truth.
  Dirty changes are coalesced with one submission in flight, while the periodic
  heartbeat resends a complete snapshot to repair dropped updates.
- The trusted supervisor stamps Worker execution and request identity on kernel
  calls; application payloads do not supply sandbox or workload identity. The
  kernel-selected database backend is available synchronously before entrypoint
  import, while Worker policy either permits or denies database operations.
  Request completion and Worker shutdown close corresponding transaction scopes.
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
  different definition is rejected, and drain waits for pending starts. Snapshot
  transmission never blocks those local state transitions.

# Public API

- `Supervisor` is the test API and `main.ts` is the image entrypoint.

# Dependencies

- Deno HTTP/Web Worker APIs, generic Worker bootstrap, and generated protocol.

# Verification

- Supervisor tests cover strict operation-specific kernel callback envelopes,
  authentication, lifecycle, stateless/persistent scheduling, strict
  bound-session follow-ups, exact reuse and completion, concurrent persistent
  database contexts, exact registered Worker invocation, streaming
  HTTP/WebSocket relay, metadata, independent session expiry and Worker idle
  time, concurrent idempotent lifecycle retries, bounded soft concurrency, logs,
  drain, bounds, cancellation, snapshot coalescing/recovery, and crash
  isolation.

# Child DOX Index
