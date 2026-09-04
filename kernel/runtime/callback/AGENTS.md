# Purpose

- Expose the private runtime-to-kernel HTTP/JSON API over one Unix socket.

# Ownership

- Own `/run/the8020/kernel.sock` host-side lifecycle, per-sandbox token
  verification, protocol envelopes, heartbeat persistence, and
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
  must agree, constant-time bearer validation uses persisted sandbox secrets,
  and terminal groups cannot be revived by late callbacks.
- Heartbeat fields are persisted through an atomic read-modify-write that
  preserves concurrent monitor fields and rechecks terminal state before write.
- Administration, typed operations, database access, and Worker invocation are
  available to both job and service Workers after exact active runtime-group,
  sandbox, Worker, execution, and workload validation. They do not require an
  HTTP request or authenticated user.
- After exact identity validation, administration and typed-operation calls
  carry their runtime execution in Go context so synchronous child jobs cannot
  queue behind the waiting parent that requested them.
- Callback identity carries the internal version-specific workload ID
  separately from the public service ID. Worker validation uses the workload
  ID; request registration, database scoping, and persistent service routing
  use the service ID.
- Authentication login/logout alone remains service-request scoped because it
  consumes the public request's security and current-user context. Password
  payloads are never logged.
- Administrative calls dispatch the existing transport-independent registry;
  typed package operations use the separate private dispatcher and never
  recurse through public package commands.
- Database calls delegate to the kernel-owned database. The backend name needed
  during module import travels in non-secret Worker metadata.
- Database transaction tokens are scoped by runtime group, sandbox, Worker,
  Worker execution, and request/job identity. Request completion closes that
  exact scope; Worker termination closes its scope prefix, rolling back leaked
  transactions. Evaluator Workers have database calls disabled.
- Worker invocation applies a five-second context and forwards one exact
  node/sandbox/Worker plus optional persistent-execution target while treating
  the registered function and JSON as opaque.
- Persistent completion derives source placement and execution identity from the
  trusted Worker call and removes only an exactly matching generic route; it
  carries no application reason.
- Production mounts the socket's containing node-private directory into every
  workload sandbox. Supervisors open a new Unix connection per call so a socket
  replaced after kernel restart reconnects without remounting an inode. JSON
  responses declare their exact content length so completion does not depend on
  Unix transport EOF propagation.

# Work Guidance

- Never log bearer tokens, passwords, or secure job inputs.

# Verification

- Unit tests cover Unix listener lifecycle/reconnect, token/protocol and
  terminal-state rejection, job/service runtime identity, authentication,
  administration and typed operations, database access, exact Worker invocation,
  and persistent completion.

# Child DOX Index
