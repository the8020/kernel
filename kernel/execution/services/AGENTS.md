# Purpose

- Own low-level sandbox-local Deno service Worker pools, capacity reconciliation,
  and supervisor dispatch.

# Ownership

- Start a service in a coordinated service group, create/configure its Worker
  pool, scale within limits, stream requests through the supervisor,
  list/inspect, and stop.
- Do not implement service program logic, buffer bodies, expose the supervisor
  control API, or create a separate service runtime.

# Local Contracts

- A typed terminal-runtime failure marks the affected pool `FAILED` and
  `runtime_unavailable` without probing or stopping vanished Workers. The next
  start releases its old sandbox ownership before recreating the same desired
  pool, so reconciliation heals explicit kills and terminal runtime failures
  without leaking stopped sandboxes.

- Public API includes lifecycle/query methods plus `EnsureCapacity`,
  cache-only `Capacity`, targeted `ListForService`, `ReconcileCapacity`,
  exact-Worker dispatch, HTTP dispatch, and WebSocket proxy.
- A logical-service-to-pool index is rebuilt once from cached recovery records
  and updated with each pool write/removal. Version cleanup never scans every
  service pool.
- Shared service groups retain separate pools keyed by service ID. Requests
  select least-in-flight eligible Workers and remain streaming across
  Go/supervisor/Worker boundaries.
- Each sandbox-local pool records internal stateless/persistent execution mode,
  concurrency per Worker, Worker minimum/maximum, target utilization, and Worker
  keepalive. Concurrency one is strict; larger values are targets with one
  bounded temporary extra request per Worker. Request admission uses the greater
  of cached supervisor occupancy and kernel reservations, grows Workers to
  preserve target headroom, and reports typed sandbox-capacity failure when the
  high-level scheduler must place capacity elsewhere.
- `Capacity` reads only the selected runtime group's cached snapshot. Lifecycle
  and reconciliation mutations use a striped service lock; unrelated pools do
  not serialize behind supervisor or sandbox I/O.
- A supervisor call made outside that striped lock must re-read and match the
  pool's runtime identity before persisting failure, so an old response cannot
  overwrite a replacement pool.
- Persistent follow-ups carrying trusted execution identity bypass new-slot
  admission and target the bound Worker. Supervisor-reported persistent
  reservations remain occupied while disconnected during keep-alive.
- An explicit instance-root-bounded development workspace becomes an
  owner-scoped runtime-profile mount at `/workspace`; incompatible workspace
  mounts split runtime groups.
- Scale-to-zero pools wake on demand and return to zero after Worker keepalive;
  saturated pools pre-scale within their sandbox-local maximum. Request-count
  recycling is not a second lifecycle policy.
- Scale and request-capacity reconciliation treat only supervisor-reported
  `ready` Workers as usable, exclude failed/draining/missing Worker IDs before
  dispatch, reap pool-owned orphans, and replace lost capacity to the requested
  count.
- Target-utilization growth is best-effort when an existing Worker still has a
  permitted execution slot: a failed headroom start returns typed capacity evidence
  so the kernel may try another sandbox and then safely use that slot. True hard
  saturation never falls through to dispatch.
- Scale-down uses supervisor-reported idle timestamps and an injected clock,
  removes only expired idle excess while retaining the minimum and target-load
  headroom, excludes Workers from supervisor scheduling before graceful stop,
  and never removes an occupied persistent slot. Full streams retain ownership
  through end/cancel.
- Pool shutdown marks the pool `DRAINING` and excludes it from dispatch. It
  stops idle Workers, returns incomplete without error while any Worker reports
  occupied execution slots, and leaves that ownership durable for
  reconciliation. Once empty, it removes every Worker, releases its sandbox
  owner, and destroys the sandbox when that was its final owner. Failed startup
  also releases partial ownership. If the runtime group was already removed,
  shutdown retires the recoverable pool index without contacting missing
  Workers. `RemoveStopped` deletes only a pool that is durably `STOPPED` with no
  recorded Workers.
- A start failure before group ownership is acquired removes its provisional
  pool record immediately; it must not leave an empty-group artifact for the
  filesystem reconciler to retry.
- A supervisor `4xx` Worker-start rejection is an invalid service definition,
  not unavailable capacity. Startup stops any partial Workers, releases the
  provisional group, and deletes the pool record when cleanup completes; only
  incomplete cleanup remains durably recoverable.
- Sandbox/supervisor failure marks every live service in the runtime group
  failed with a durable runtime-unavailable marker. Subsequent shutdown clears
  its Worker indexes and releases ownership without calling the dead group.
- Startup reconciliation may call `RetireUnavailable` for an exact pool whose
  sandbox is authoritatively absent; it clears Worker indexes and persists
  `STOPPED` without probing the vanished runtime.
- Restart restoration marks a persisted pool failed when its runtime group is
  unavailable or any recorded Worker is no longer `ready`, allowing the
  filesystem reconciler to recreate it immediately.
- Invalid records are quarantined individually; Worker restoration failures mark
  only their owning pool failed and never roll back healthy restored pools.

# Work Guidance

- Persist desired service and Worker-pool identity before/after mutations, keep
  scaling deterministic by Worker ID, and keep capacity reconciliation scoped
  to the selected pool.

# Verification

- Unit tests cover start/minimum pool, shared pools, stateless/persistent mode,
  strict and bounded-soft capacity, cached reserved-demand/target scaling,
  fake-clock Worker keepalive,
  target-headroom scale-down and growth fallback, exclude-before-stop ordering,
  failed-Worker repair, streamed dispatch, exact-Worker routing, resumable stop,
  owner release, group failure, mixed valid/corrupt recovery, isolated
  restoration, occupied-Worker draining, missing-group retirement,
  terminal-record removal, rejected-definition cleanup, idempotent
  already-absent group release, and rollback.

# Child DOX Index
