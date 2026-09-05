Parent DOX: [kernel/kernel/sandbox DOX](../AGENTS.md).

# Purpose

- Coordinate the authoritative sandbox lifecycle across durable state, selected
  networking/backend, supervisor health, and backend metrics.

# Ownership

- Allocate collision-checked compact sandbox IDs, create runtime groups
  transactionally, assign only clean healthy warm groups, wait for supervisor
  readiness, inspect/list/measure, combine task/heartbeat/available-OOM health,
  terminate failed groups and archive their evidence, drain/stop/kill/delete,
  reconcile after restart, remove owned orphans, and apply startup, shutdown,
  and history-retention policies.
- Do not select grouping keys, schedule workload Workers, implement
  containerd/CNI details, allocate ports, or render commands.

# Local Contracts

- Public API: `New`, `NewSandboxID`, `ReleaseSandboxID`, `Manager.Create`,
  `AssignWarm`, `AddOwner`, `RemoveOwner`, `Capacity`, `List`,
  `ResolveRuntimeGroup`, `Inspect`, `Refresh`, `Metrics`, `OpenConsole`,
  `CheckHealth`, `Stop`, `Kill`, `Delete`, `ListHistory`, `InspectHistory`,
  `CleanupHistory`, `Reconcile`, `Startup`, and `Shutdown`.
- Reconcile startup restores healthy persisted runtime groups and deletes owned
  backend orphans. Default destroy startup bypasses all health and supervisor
  probes, force-deletes every instance-owned backend object from ownership
  metadata, releases matching networks/ports, and removes persisted
  runtime-group records.
- Desired files are written before side effects; observed state changes only
  through explicit model transitions; backend, network, supervisor, and resource
  evidence remain separate.
- Creation rolls back container/network side effects on failure. Stop, kill,
  failure, reconciliation rejection, and deletion revoke matching host-port
  leases; completed cleanup moves terminal metadata and logs into history before
  removing the live runtime-group record.
- Successfully archived records are physically absent from live listing;
  cleanup-pending terminal records remain live and inspectable until explicit
  retry succeeds. History listing is a separately requested bounded index query;
  inspection uses a history ID and never searches live state.
- Heartbeat callbacks maintain an indexed in-memory deadline queue. Monitoring
  claims at most 256 stale candidates per pass without scanning the sandbox
  catalog, performs targeted backend/cgroup inspection only for those entries,
  and conditionally publishes failure against the newest heartbeat so a
  concurrent fresh callback cannot be overwritten or killed by an older
  observation.
- Ordinary `List` and `Inspect` combine recovery status with the latest cached
  supervisor snapshot and perform no supervisor/backend I/O. `Refresh`
  explicitly samples one selected sandbox's absolute supervisor snapshot and raw
  CPU, memory, and process diagnostics concurrently, then updates the memory
  cache. Resource observations never become placement utilization.
- Exact runtime-group resolution returns only the persisted specification and
  never contacts the supervisor or backend; identity-routing callers use it when
  they already hold the runtime-group ID.
- Shared-group reuse durably adds each distinct owner and logical service to
  specification/status state and container labels. Removing an owner updates
  those indexes transactionally and deletes the sandbox when no owner remains.
  Label patches contain only workload-applicable non-empty fields; job groups
  never manufacture an empty service label.
- Creation admits declared reservations only while node-wide sandbox-count and
  temporary-storage budgets remain. `Capacity` exposes those limits and current
  reservations without inferring health from usage.
- Capacity admission is reserved under one short lock. Runtime-group lifecycle
  operations use striped locks and never serialize unrelated backend, network,
  supervisor, or filesystem I/O. Admission reads only preloaded live/history
  identity indexes and cached capacity while holding its lock. Explicit global
  reconciliation excludes only another reconciliation; heartbeat health work has
  no global lock. Dependency calls remain context bounded; readiness failures
  preserve cancellation or deadline while retaining the last probe error as
  diagnostics.
- `OpenConsole` resolves only a persisted ready sandbox and requires the
  selected production backend's optional console capability.

# Work Guidance

- Keep backend/network/supervisor/port-cleanup dependencies narrow and fakeable.
  Never repair a missing runtime by running Deno on the host.

# Verification

- Unit tests cover successful creation, compact ID reservation, readiness,
  generic console routing, node-budget admission, warm assignment/shared-owner
  add/remove and final-owner destruction, failure archival, lifecycle
  transitions, sandbox-scoped port release, Worker-count and raw CPU/RAM
  metrics, cache-only healthy monitoring, heartbeat timeout, stale-sandbox OOM
  evidence preservation/full cleanup, deletion, reconstruction, cached versus
  targeted live inspection, parallel unrelated creations, missing groups, owned
  orphans, and startup/shutdown policies.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
