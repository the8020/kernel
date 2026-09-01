# Purpose

- Own filesystem-service reconciliation, canonical HTTP/WebSocket routing,
  session execution routes, autoscaling coordination, and administration.

# Ownership

- Reconcile shared package desired state into node-local low-level service
  sandbox allocations, perform rolling generation replacement, retain healthy generations
  on replacement failure, and publish node-local observed status.
- Enforce public/authenticated access, strip canonical service prefixes and
  untrusted internal headers, attach trusted request/auth/execution metadata,
  stream HTTP bodies, proxy WebSockets, and select local or remote capacity.
- Own the shared opaque session-route registry and route follow-up work to an
  exact node, sandbox, Worker, and execution context.
- Do not parse manifests, execute application handlers, interpret UUI messages,
  schedule inside a sandbox-local pool, or implement sandbox lifecycle.

# Local Contracts

- Canonical service lifecycle is `stateless` or `session`; the latter translates
  once to the supervisor's generic internal `persistent` execution mode. HTTP,
  streaming, SSE, and WebSocket routing use the same boundary in either mode.
- A persistent first request creates a high-entropy `X-80-20-Route`; only its
  SHA-256 key is persisted in shared `state/services/persistent-routes.json`.
  Routes are service- and authenticated-user-bound and resolve to node, pool,
  Worker, and logical execution. Missing, expired, or lost routes return a
  conflict and never silently create replacement state.
- Ordinary clients reuse the route header. Browser WebSockets first establish
  through HTTP, then carry the same token as `?route=` because browser APIs
  cannot set/read arbitrary upgrade headers. The query is removed before Worker
  dispatch and route values are never logged.
- Active HTTP streams and WebSockets refresh their route lease; a stopped or
  crashed owning transport stops refreshing, so even a previously connected
  route expires after session keepalive. Worker keepalive does not extend it.
- A local persisted route is valid only while its exact pool and Worker remain
  present. Kernel restart or Worker replacement invalidates a route whose old
  Worker identity is gone and returns `409` so ordinary clients can establish a
  replacement instead of looping on a supervisor `503`.
- A generic authenticated persistent-completion callback removes only the route
  for the caller's exact service, Worker, and logical execution. The kernel does
  not receive or infer an application termination reason.
- Stateless requests choose least-loaded local sandboxes and Workers. Session
  follow-ups target the recorded Worker; initial session work reserves one
  logical execution slot through the generic supervisor contract.
- Autoscaling computes desired Workers from kernel-reserved demand, concurrency,
  and target utilization, then packs Workers into compatible existing sandboxes
  before provisioning another. A narrow per-service capacity lock prevents
  concurrent requests or reconciliation from exceeding a finite maximum.
  Per-service Workers-per-sandbox and the kernel-wide per-sandbox Worker limit
  are enforced; remote spillover follows local admission failure. Saturation returns
  structured `503` capacity diagnostics.
- Minimum-worker reconciliation uses the same one-Worker-at-a-time placement:
  it retains successful Workers, packs each eligible sandbox up to service and
  kernel Worker limits, and spills a typed Worker rejection into another
  same-group sandbox. Per-sandbox floors are then derived from the observed
  distribution so global minimum capacity is not churned back into a full
  sandbox.
- If target-headroom growth fails while an existing Worker still has a hard
  slot, placement first tries compatible new capacity and then dispatches
  through that safe fallback. Transient resource pressure must not turn an
  available slot into an unbounded sandbox-provisioning loop.
- Scale-down removes only excess idle Workers after Worker keepalive, never
  below the minimum or target-load headroom. Empty excess sandboxes may then be
  retired, while minimum sandboxes remain warm independently of minimum Workers
  and active session routes prevent retirement. Releasing the final sandbox
  owner destroys that sandbox.
- Minimum-sandbox allocation indexes are partitioned deterministically across
  enabled nodes and same-generation reconciliation moves allocations when that
  assignment changes. Sandbox and Worker counts in service status remain
  node-local while node capacity reports provide cluster-facing inventory.
- Initial zero-capacity failure is `PENDING_CAPACITY`; partial minimum capacity
  is `DEGRADED`; desired state remains intact and reconciliation retries. A cold
  request may use healthy degraded capacity. Same-generation retries add only
  missing assigned sandboxes or Workers in place and never drain healthy
  capacity or invalidate session routes merely because a configured minimum is
  temporarily unavailable.
- A supervisor-rejected application definition marks that exact generation
  stably `FAILED` or `DEGRADED`. Maintenance and cold requests do not compile or
  retry the same rejected generation; an explicit restart or a new generation
  clears the rejection and attempts it once. Capacity and infrastructure
  failures remain retryable `PENDING_CAPACITY` behavior.
- Startup performs one fixed-depth package/service discovery. Periodic
  maintenance reads only services with live runtime sandboxes or pending
  capacity; it never rediscovers the complete package catalog. Explicit service
  mutations, requests, and `ReconcileAll` reconcile immediately.
- Repeated identical maintenance failures neither increment failure counters nor
  emit duplicate logs until the failure changes or clears.
- Replacement capacity is validated before a generation switch. Persistent
  routes may retain old-generation pools until their executions expire; all
  other stale pools drain and are retried on cleanup failure. Fully stopped
  stale-generation pool records are removed so destroyed prior-generation
  sandboxes cannot cause permanent reconciliation retries.
- Package synchronization increments every current service generation through
  `Reload` and uses `Retire` to stop and forget runtime capacity for services no
  longer declared by the package. Shared service desired state remains intact
  so restoring a prior package version can reconcile it again.
- Entrypoint/OpenAPI validation uses an isolated temporary pool and removes its
  terminal record after validation; reconciliation garbage-collects stopped
  validation records left by earlier kernels.
- This package forwards no application settings and performs no application
  inventory or Worker scan. Package-owned administration may use the generic
  exact-Worker invocation capability through the kernel SDK.
- Request and response bodies remain streaming. Canonical path validation
  rejects encoded separators, backslashes, nulls, traversal, invalid UTF-8, and
  client-supplied internal headers.

# Work Guidance

- Re-read exact manifests for reconciliation and every external request. Keep
  registries limited to active runtime identity, route leases, and counters.

# Verification

- `webservices_test.go` and `persistent_routes_test.go` cover canonical and
  authenticated routing, streaming, generic HTTP/WebSocket persistence, exact
  Worker reuse, shared token-safe routes, crash expiry, node forwarding,
  assigned sandbox indexes, reserved-demand Worker scaling, finite maximums,
  fake-clock Worker keepalive, minimum Worker and sandbox floors, compatible
  sandbox packing and Worker-limit-triggered minimum spillover, per-service capacity
  locking, failed cold-start rollback,
  idle sandbox scale-down,
  generation replacement, degraded cold-start routing, in-place missing-capacity
  recovery, capacity states, stale-pool cleanup, terminal pool-record removal,
  validation-pool cleanup, stale persistent-Worker rejection, exact persistent
  completion, one-time catalog discovery, active-only background maintenance,
  stable rejected-generation suppression with explicit retry, and duplicate
  failure suppression.

# Child DOX Index
