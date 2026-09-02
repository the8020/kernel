# Purpose

- Receive authenticated supervisor registration and heartbeat callbacks from full sandbox networks or rootless loopback endpoints.

# Ownership

- Own the bounded internal HTTP listener, durable per-mode endpoint selection,
  selected-network source restriction, per-sandbox token verification, protocol
  envelope validation, observed heartbeat persistence, and supervisor-mediated
  authentication, administrative, database, exact-Worker invocation, and
  persistent completion calls tied to trusted service execution context.
- Do not expose public APIs, control Workers, probe containerd, allocate networking, or replace direct supervisor health checks.

# Local Contracts

- Public API: `New`, `Server.Start`, `Address`, and `Close`.
- Only generated registration, heartbeat, bootstrap-authentication,
  administrative, database, Worker-invocation, and persistent-completion envelopes are
  accepted; envelope/payload versions and runtime-group identity must agree,
  constant-time bearer validation uses persisted sandbox secrets, and terminal
  groups cannot be revived by late callbacks.
- Heartbeat fields are persisted through an atomic read-modify-write that
  preserves concurrent monitor fields and rechecks terminal state before write.
- Authentication and administrative calls additionally require service workload identity, execution/Worker/sandbox/service/request identifiers, a matching active kernel request registration, and a correlation ID; password payloads are never logged.
- Administrative calls require the active request's kernel-trusted bootstrap-administrator identity and dispatch the existing transport-independent command registry without duplicating handlers.
- Database calls require the supervisor-provided active service request or job
  execution context and delegate to the kernel-owned database. The backend name
  needed during module import travels in non-secret Worker metadata instead of a
  callback.
- Database transaction tokens are scoped by runtime group, sandbox, Worker,
  Worker execution, and request/job identity. Request completion closes that
  exact scope; Worker termination closes its scope prefix, rolling back leaked
  transactions. Evaluator Workers have database calls disabled.
- Worker invocation additionally requires an authenticated active request,
  applies a five-second context, and forwards one exact node/sandbox/Worker
  target while treating the registered function and JSON as opaque.
- Persistent completion derives source placement and execution identity from
  the trusted Worker call and removes only an exactly matching generic route;
  it carries no application reason.
- Production startup atomically persists the selected listener port and rebinds it after kernel restart so surviving supervisors retain a valid callback address; corrupt or occupied endpoint state fails explicitly.

# Work Guidance

- Full mode advertises the CNI bridge gateway; rootless mode binds and advertises loopback. Never log tokens or accept sources outside the configured network.

# Verification

- Unit tests cover registration/heartbeat persistence, source/token/protocol
  rejection, terminal-state rejection, authentication/admin calls, exact
  Worker invocation authorization/bounds, persistent completion identity,
  listener lifecycle, restrictive endpoint state, and exact port reuse.

# Child DOX Index
