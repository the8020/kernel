# Purpose

- Own authenticated Go-kernel communication with one Deno supervisor inside a
  sandbox.

# Ownership

- Query aggregate ready/failed/active status and Workers with explicit failure
  state, last-idle time, and bounded identity-associated logs; start and stop
  Workers; send exact registered-function invocations, return job results with
  structured logs, configure service pools, stream service requests with trusted
  selected-Worker identity, and request drain.
- Do not create sandboxes, select groups, expose host ports, interpret program
  results, or access containerd.

# Local Contracts

- Public API: `New`, `Client` control methods, request/status types, and
  `EndpointForSandbox`; default endpoint resolution honors each sandbox's
  allocated supervisor port.
- Every request carries the per-sandbox bearer token and is context bounded;
  lifecycle control requests and responses use the generated versioned envelope
  and must match message type, runtime-group identity, and correlation ID.
- Service request and response bodies remain streams and are never converted to
  JSON or fully buffered; service redirects are returned unchanged and are never
  followed on the private supervisor hop.
- Service WebSocket proxying preserves the original relative URL, subprotocols,
  and trusted metadata while authenticating the private supervisor upgrade with
  the sandbox token.
- Exact Worker invocation targets one known Worker and carries a bounded
  application-defined function name, optional persistent-execution identity, and
  JSON input/output without scanning, interpreting the name, or exposing the
  private endpoint publicly.
- Non-success control responses retain their bounded HTTP status in
  `ResponseError`; callers may classify a `4xx` response as a rejected request
  without parsing error text, while authentication tokens remain hidden.

# Work Guidance

- Keep JSON control bodies small, cap error bodies, preserve Worker idle
  timestamps exactly, and return precise remote status errors without exposing
  tokens.

# Verification

- Unit tests cover authentication, generated control-envelope types/correlation,
  identity/version validation, Worker invocation, job/service routes, streaming
  bodies, unchanged redirects, WebSocket URL/authentication preservation,
  bounds, typed remote rejection errors, and cancellation.

# Child DOX Index
