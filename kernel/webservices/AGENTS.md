Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own package-service reconciliation, canonical HTTP/WebSocket routing, session
  execution routes, autoscaling coordination, and administration.

# Ownership

- Reconcile accepted runtime specifications into node-local low-level service
  sandbox allocations, perform rolling version replacement, retain healthy
  versions on replacement failure, and publish node-local observed status.
- Enforce public/authenticated access, strip canonical service prefixes and
  untrusted internal headers, attach trusted request/auth/execution metadata,
  stream HTTP bodies, proxy WebSockets, and select local or remote capacity.
- Issue and verify signed route descriptors, then dispatch to exact node,
  sandbox, Worker, and persistent execution identities. No database dependency
  or route registry exists here; the supervisor owns execution lifetime.
- Do not parse manifests, execute application handlers, interpret UUI messages,
  schedule inside a sandbox-local pool, or implement sandbox lifecycle.

# Local Contracts

- Canonical service lifecycle is `stateless` or `session`; the latter translates
  once to the supervisor's generic internal `persistent` execution mode. HTTP,
  streaming, SSE, and WebSocket routing use the same boundary in either mode.
- Initial HTTP or WebSocket responses sign `the8020-route+jwt` only after the
  exact Worker is known, using the existing deployment signer. The descriptor
  contains node, sandbox, Worker, and persistent execution IDs. Routing and
  authentication validators reject each other's JWT type.
- Ordinary clients reuse `the8020-route`; browser WebSockets reuse it as
  `?route=` after HTTP establishment. Remove the query and header before Worker
  dispatch and never log tokens. Signed routes require no table, token hash,
  kernel lease timer, denylist, or refresh protocol.
- Follow-ups resolve node IDs through topology without cold allocation. Local
  pools prove membership in the service selected by the URL; the supervisor
  proves the exact binding exists and belongs to the current principal.
  Missing/expired/completed bindings return `409` and never recreate state.
  Replacement runtime IDs cannot inherit stale routes. Unknown transport
  outcomes and temporarily unavailable nodes stay transport failures; no replay
  or replacement execution is attempted.
- The supervisor holds bindings through HTTP/stream/SSE/WebSocket lifetime and
  disconnect keepalive, and releases them on expiry or explicit local
  completion. Cached absolute occupancy drives kernel capacity, draining, and
  retirement; there is no duplicate route lease or completion RPC back to Go.
- Headers use lowercase `the8020-*` names in source. All private transport
  metadata uses `the8020-internal-`; stripping compares lowercase names even
  when net/http canonicalizes them. Worker responses cannot forge route headers
  or expose private metadata. WebSocket response modification runs before the
  proxy sends upgrade headers, preserving streaming and the selected target.
- Stateless requests choose least-loaded local sandboxes and Workers. Session
  follow-ups target the recorded Worker; initial session work reserves one
  logical execution slot through the generic supervisor contract.
- Public dispatch never reads or verifies platform credentials, evaluates an
  account/session, or supplies an authentication hook. It uses the configured
  execution principal and preserves credentials as ordinary unverified request
  data. Authenticated dispatch uses `kernel/auth` JWT verification before cold
  reconciliation, capacity, persistent binding, or Worker dispatch. An explicit
  `the8020-authorization` header wins over `the8020_auth`, including when
  invalid. Rejection uses the service's existing unauthenticated response and
  clears a rejected selected browser cookie. Client-forged internal headers are
  stripped.
- Successful verification carries claims, the composition-selected package hook,
  and the existing unauthenticated response policy as trusted request metadata.
  It never marks application authentication complete. The target Worker approves
  user/session policy before handler/upgrade; kernel routing and callbacks
  retain the signed principal. Go never queries the users package tables.
- Warm routing uses one immutable definition lookup, one cache-only supervisor
  capacity read per candidate sandbox, a short reservation, and final dispatch.
  It performs no manifest read, Worker scan, live supervisor inspection, metrics
  probe, or capacity reconciliation. Reservations are separate from observed
  state, expire after 30 seconds if cleanup is abandoned, and never appear in
  administration; newer supervisor snapshots dominate them for routing truth.
- Autoscaling computes desired Workers from reserved demand, concurrency, and
  target utilization, then packs Workers into compatible existing sandboxes
  before provisioning another. A narrow per-service capacity lock deduplicates
  scale-up without serializing unrelated services. Maximum Workers and the
  kernel-wide per-sandbox Worker count remain hard. Concurrency and
  Workers-per-sandbox equal to one are strict; larger values are balancing/
  packing targets with small bounded race overshoot. Remote spillover follows
  local admission failure, and saturation returns structured `503` diagnostics.
- Minimum-worker reconciliation uses the same one-Worker-at-a-time placement: it
  retains successful Workers, packs each eligible sandbox up to service and
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
  enabled nodes and same-version reconciliation moves allocations when that
  assignment changes. Sandbox and Worker counts in service status remain
  node-local while node capacity reports provide cluster-facing inventory.
- Initial zero-capacity failure is `PENDING_CAPACITY`; partial minimum capacity
  is `DEGRADED`; desired state remains intact and reconciliation retries. A cold
  request may use healthy degraded capacity. Same-version retries add only
  missing assigned sandboxes or Workers in place and never drain healthy
  capacity or invalidate session routes merely because a configured minimum is
  temporarily unavailable.
- A supervisor-rejected application definition marks that exact version stably
  `FAILED` or `DEGRADED`. Maintenance and cold requests do not compile or retry
  the same rejected version; an explicit restart or a new version clears the
  rejection and attempts it once. Capacity and infrastructure failures remain
  retryable `PENDING_CAPACITY` behavior.
- Startup consumes the accepted service runtime index. Periodic maintenance
  consumes at most 256 deduplicated queued services with live runtime sandboxes,
  draining pools, or pending capacity; it never scans the service/package
  catalogs or every runtime pool. Failed retirement remains in this same queue
  until stopped pools and observed records are removed. Explicit service
  mutations, requests, and `ReconcileAll` reconcile immediately.
- Active request routing uses the immutable definition snapshot belonging to the
  loaded Worker version; it never reparses manifests or scans shared state on
  the hot path. Cold starts and reconciliation read the accepted runtime
  specification before allocating capacity.
- Background capacity reconciliation is single-flight per service, owned by the
  manager lifecycle, cancelled during close, and joined before shutdown returns.
- Service reconciliation uses the same per-service capacity lock and holds no
  process-wide mutex across sandbox or supervisor I/O, so unrelated services can
  start, stop, and recover concurrently.
- Repeated identical maintenance failures neither increment failure counters nor
  emit duplicate logs until the failure changes or clears.
- Replacement capacity is validated before a version switch. HTTP, WebSocket,
  and persistent follow-up routing select only sandboxes from the loaded
  generation. Every stale pool follows the same drain workflow: it receives no
  new routed work, occupied Workers remain `DRAINING` without making the switch
  fail, and maintenance retries until it can remove the fully stopped record.
- `Index.ReplacePackage` validates the entire fragment, then replaces it under
  one short publication lock and reports removed IDs for retirement. Hook or
  specification failure leaves the old fragment untouched. The owning reindex
  path handles boot, activation, removal, edits, and revision convergence.
- Entrypoint/OpenAPI validation uses an isolated temporary pool and removes its
  terminal record after validation; reconciliation garbage-collects stopped
  validation records left by earlier kernels.
- Service status is one logical row per service. It reports all live current or
  draining-version sandboxes and Workers by unique identity, counts distinct
  versions, and includes each sandbox's version; routing still targets only the
  loaded version. Request metrics belong to the stable logical service so a
  request finishing on a draining version updates the same aggregate.
- Administration replaces routing reservations with cached supervisor-observed
  request/Worker totals and exposes snapshot revision/time. Explicit service
  refresh inspects only that service's unique sandboxes with at most eight
  concurrent probes; list and ordinary detail reads stay cache-only.
- This package forwards no application settings and performs no application
  inventory or Worker scan. Package-owned administration may use the generic
  exact-Worker invocation capability through the kernel SDK.
- Request and response bodies remain streaming. Canonical path validation
  rejects encoded separators, backslashes, nulls, traversal, invalid UTF-8, and
  client-supplied internal headers.
- Trusted request metadata includes the normalized IP address observed on the
  kernel socket and its loopback, private, link-local, public, or special
  network scope. Client-supplied internal address metadata is discarded; proxy
  forwarding headers are not trusted without an explicit trusted-proxy policy.

# Work Guidance

- Read only accepted runtime specifications during routing/reconciliation. Keep
  runtime snapshots limited to observed execution state and keep reservations
  private to routing.

# Verification

- `webservices_test.go` and `persistent_routes_test.go` cover canonical and
  authenticated routing, streaming, generic HTTP/WebSocket persistence, exact
  Worker reuse, signed exact-target routes, supervisor expiry, node forwarding,
  assigned sandbox indexes, reserved-demand Worker scaling, finite maximums,
  fake-clock Worker keepalive, minimum Worker and sandbox floors, compatible
  sandbox packing and Worker-limit-triggered minimum spillover, per-service
  capacity locking, concurrent unrelated warm dispatch, strict/bounded-soft
  dispatch, failed-dispatch reservation release, abandoned-reservation expiry,
  cache-only warm routing, failed cold-start rollback, idle sandbox scale-down,
  version replacement, current-generation-only routing with prior-version
  draining, degraded cold-start routing, in-place missing-capacity recovery,
  capacity states, stale-pool cleanup, terminal pool-record removal,
  validation-pool cleanup, stale persistent-Worker rejection, exact persistent
  completion, accepted-index discovery, bounded background maintenance, stable
  rejected-version suppression with explicit retry, and duplicate failure
  suppression.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
