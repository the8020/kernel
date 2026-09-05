Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Own authenticated Go-kernel communication with one Deno supervisor inside a
  sandbox.

# Ownership

- Query an absolute revisioned runtime snapshot, aggregate ready/failed/active
  status, and Workers with explicit failure state, last-idle time, and bounded
  identity-associated logs; start and stop Workers; send exact
  registered-function invocations, return job results with structured logs,
  configure service pools, stream service requests with trusted selected-Worker
  identity, and request drain.
- Do not create sandboxes, select groups, expose host ports, interpret program
  results, or access containerd.

# Local Contracts

- Public API: `New`, `Client` control methods, request/status types, and
  `EndpointForSandbox`; default endpoint resolution honors each sandbox's
  allocated supervisor port.
- Every request carries the per-sandbox bearer token and is context bounded;
  lifecycle control requests and responses use the generated versioned envelope
  and must match message type, runtime-group identity, and correlation ID.
- Job arguments are always encoded as a JSON array, including an empty array for
  a no-argument program.
- Service request and response bodies remain streams and are never converted to
  JSON or fully buffered; service redirects are returned unchanged and are never
  followed on the private supervisor hop.
- Service WebSocket proxying preserves the original relative URL, subprotocols,
  and trusted metadata while authenticating the private supervisor upgrade with
  the sandbox token. The caller may modify the upstream response before public
  headers or upgrade bytes are sent, allowing exact-target route signing through
  the same streaming proxy. The selected Worker uses
  `the8020-internal-selected-worker-id` for both HTTP and WebSocket responses.
- Exact Worker invocation targets one known Worker and carries a bounded
  application-defined function name, optional persistent-execution identity, and
  validated effective user plus JSON input/output without scanning, interpreting
  the name, or exposing the private endpoint publicly.
- Worker startup requires a canonical user and a workload-compatible service,
  job, or program origin; neither is optional runtime metadata.
- Non-success control responses retain their bounded HTTP status in
  `ResponseError`, including a structured execution code/details when supplied;
  callers may classify a `4xx` response without parsing error text, while
  authentication tokens remain hidden.
- `Snapshot` validates protocol version, sandbox/runtime-group/workload
  identity, and a nonzero revision before returning the supervisor's absolute
  observation. Routine routing reads the kernel snapshot cache; this live RPC is
  reserved for targeted refresh and recovery.

# Work Guidance

- Keep JSON control bodies small, cap error bodies, preserve Worker idle
  timestamps exactly, and return precise remote status errors without exposing
  tokens.

# Verification

- Unit tests cover authentication, snapshot identity/revision validation,
  generated control-envelope types/correlation, identity/version validation,
  Worker invocation, job/service routes, streaming bodies, unchanged redirects,
  WebSocket URL/authentication preservation, bounds, typed remote rejection
  errors, and cancellation.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
