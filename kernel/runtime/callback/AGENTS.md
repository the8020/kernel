# Purpose

- Expose the private runtime-to-kernel HTTP/JSON API over one Unix socket.

# Ownership

- Own `/run/the8020/kernel.sock` host-side lifecycle, per-sandbox token
  verification, protocol envelopes, in-memory runtime snapshots, and
  supervisor-mediated authentication, administration, typed runtime operations,
  database, exact-Worker invocation, and persistent completion calls.
- Do not expose public APIs, control Workers, probe containerd, allocate
  networking, or replace direct supervisor health checks.

# Local Contracts

- Public API: `New`, `Server.Start`, runtime dependency setters,
  `Server.Address`, and `Close`.
- Only generated registration, heartbeat, authentication,
  administrative, database, Worker-invocation, and persistent-completion
  envelopes are accepted; envelope/payload versions and runtime-group identity
  must agree, constant-time bearer validation uses the state store's preloaded
  token cache, unknown identities remain cache-only misses, and terminal groups
  cannot be revived by late callbacks.
- Registration and heartbeat carry one absolute revisioned supervisor snapshot.
  Applying a snapshot and refreshing heartbeat freshness are memory-only; stale
  revisions cannot replace newer state, and restart recovery obtains fresh
  truth from the supervisor heartbeat.
- Administration, typed operations, database access, and Worker invocation are
  available to both job and service Workers after cached runtime-group token
  validation and required nonempty execution/request identity. They do not
  reverse-query the supervisor or scan Workers per call.
- Administration and typed-operation calls carry the trusted Worker execution
  in Go context so synchronous child jobs cannot queue behind the waiting parent
  that requested them.
- Sandbox and workload identity derive from the authenticated runtime envelope.
  Payloads carry only Worker execution and request identity plus fields owned by
  the selected operation; service identity appears only where persistent routing
  requires it.
- Authentication login/logout alone remains service-request scoped because it
  consumes the public request's security and current-user context. Password
  payloads are never logged, and the callback request context reaches all
  authentication database work without compatibility fallbacks.
- Administrative calls dispatch the existing transport-independent registry;
  typed package operations use the separate private dispatcher and never
  recurse through public package commands.
- Database calls delegate to the kernel-owned database. The backend name needed
  during module import travels in non-secret Worker metadata.
- Database transaction tokens are scoped by runtime group, sandbox, Worker,
  Worker execution, and request/job identity. Request completion closes that
  exact scope; Worker termination closes its scope prefix, rolling back leaked
  transactions. These checks are in memory and never validate Worker liveness.
  Evaluator Workers have database calls disabled.
- Worker invocation applies a five-second context and forwards one exact
  node/sandbox/Worker plus optional persistent-execution target while treating
  the registered function and JSON as opaque.
- Persistent completion derives source placement and execution identity from the
  trusted Worker call and removes only an exactly matching generic route; it
  carries no application reason.
- Production mounts the socket's containing node-private directory for the
  trusted supervisor, while application Worker permissions deny both the socket
  and token. Supervisors open a new Unix connection per call so a socket replaced
  after kernel restart reconnects without remounting an inode. JSON responses
  declare their exact content length so completion does not depend on Unix
  transport EOF propagation. Closing a cancelled connection cancels the Go
  request context.

# Work Guidance

- Never log bearer tokens, passwords, or secure job inputs.

# Verification

- Unit tests cover Unix listener lifecycle/reconnect, token/protocol and
  terminal-state rejection, memory-only revisioned snapshots, job/service
  runtime identity, authentication, administration and typed operations,
  concurrent database access, exact Worker invocation, and persistent completion.

# Child DOX Index
