# Purpose

- Own low-level Deno service-replica Worker pools, capacity reconciliation,
  supervisor dispatch, and legacy Go-hosted exposure primitives.

# Ownership

- Start a service in a coordinated service group, create/configure its Worker
  pool, scale within limits, register/unregister path-prefix routes, optionally
  allocate a host HTTP port, stream requests through the supervisor,
  list/inspect, and stop.
- Do not implement service program logic, buffer bodies, expose the supervisor
  control API, or create a separate service runtime.

# Local Contracts

- Public API includes lifecycle/query/exposure methods plus `EnsureCapacity`,
  `ReconcileCapacity`, exact-Worker dispatch, HTTP dispatch, and WebSocket
  proxy.
- Shared service groups retain separate pools keyed by service ID. Requests
  select least-in-flight eligible Workers and remain streaming across
  Go/supervisor/Worker boundaries.
- Each replica records stateless/persistent mode, hard concurrency per Worker,
  Worker minimum/maximum, and target utilization. Request admission grows
  Workers to preserve target headroom and reports typed replica-capacity failure
  when the high-level scheduler must add a replica.
- Persistent follow-ups carrying trusted execution identity bypass new-slot
  admission and target the bound Worker. Supervisor-reported persistent
  reservations remain occupied while disconnected during keep-alive.
- An explicit instance-root-bounded development workspace becomes an
  owner-scoped runtime-profile mount at `/workspace`; incompatible workspace
  mounts split runtime groups.
- Exposure handlers are owned by Go, route by a canonical path prefix, and
  optional host listeners use the port manager; rollback removes partial
  routes/leases and restart restoration reattaches Go HTTP handlers.
- Ephemeral pools wake from zero on demand and return to zero after idle
  timeout; saturated pools pre-scale within their maximum, completed requests
  are counted, and Workers are replaced after the configured recycle count.
- Scale and request-capacity reconciliation treat only supervisor-reported
  `ready` Workers as usable, exclude failed/draining/missing Worker IDs before
  dispatch, reap pool-owned orphans, and replace lost capacity to the requested
  count.
- Scale-down uses kernel-owned cooldown/hysteresis, removes only idle Workers,
  excludes them from supervisor scheduling before graceful stop, and never
  removes an occupied persistent slot. Full streams retain ownership through
  end/cancel.
- Pool shutdown excludes every Worker from dispatch, durably removes each
  terminated Worker, releases its sandbox owner, and destroys the sandbox when
  that was its final owner. Failed startup also releases partial ownership. If
  the runtime group was already removed, shutdown retires the recoverable pool
  index without contacting missing Workers. `RemoveStopped` deletes only a pool
  that is durably `STOPPED` with no recorded Workers.
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
  sandbox is authoritatively absent; it clears Worker, route, and lease indexes
  and persists `STOPPED` without probing the vanished runtime.
- Restart restoration marks a persisted pool failed when its runtime group is
  unavailable or any recorded Worker is no longer `ready`, allowing the
  filesystem reconciler to recreate it immediately.
- Invalid records are quarantined individually; route, Worker, and host-port
  restoration failures mark only their owning pool failed and never roll back
  healthy restored pools.

# Work Guidance

- Persist desired service and Worker-pool identity before/after mutations and
  keep scaling deterministic by Worker ID.

# Verification

- Unit tests cover start/minimum pool, shared pools, stateless/persistent mode,
  hard capacity, target scaling and hysteresis, exclude-before-stop ordering,
  failed-Worker repair, streamed dispatch, exact-Worker routing, resumable stop,
  owner release, group failure, mixed valid/corrupt recovery, isolated
  restoration, missing-group retirement, terminal-record removal, and
  rejected-definition cleanup, idempotent already-absent group release, and
  rollback.

# Child DOX Index
