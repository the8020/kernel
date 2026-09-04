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
  `ResolveRuntimeGroup`, `Inspect`, `Metrics`, `OpenConsole`, `CheckHealth`,
  `Stop`, `Kill`, `Delete`, `ListHistory`, `InspectHistory`, `CleanupHistory`,
  `Reconcile`, `Startup`, and `Shutdown`.
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
- Heartbeat monitoring merges metrics into the newest status and conditionally
  publishes failure against the newest heartbeat, so a concurrent fresh callback
  cannot be overwritten or killed by a stale monitor snapshot.
- Inspection combines supervisor Worker count with raw backend CPU, memory, and
  process samples. Resource observations are diagnostic only and are not
  converted into placement utilization.
- Inspection probes runtime-local Workers and metrics only while the persisted
  lifecycle is ready, active, or draining. Terminal and transitional sandboxes
  remain inspectable from authoritative persisted state without contacting a
  supervisor that cannot be available.
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
- All public operations are synchronized; dependency calls are context bounded.
  Readiness failures preserve the governing cancellation or deadline while
  retaining the last probe error as diagnostics.
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
  metrics, heartbeat timeout, OOM evidence preservation/full cleanup, deletion,
  reconstruction, missing groups, owned orphans, and startup/shutdown policies.

# Child DOX Index
