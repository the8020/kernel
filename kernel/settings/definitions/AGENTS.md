Parent DOX: [kernel/kernel/settings DOX](../AGENTS.md).

# Purpose

- Store the authoritative modular TOML definitions compiled into the kernel.

# Ownership

- Own keys, types, required node/global storage, defaults, environment
  variables, constraints, runtime/restart metadata, and descriptions for 80|20
  settings.
- `network/` owns node-local main/SSH/HTTPS ports and the global root alias;
  `logging/` owns unified kernel/Deno persistence; `sandbox/` owns backend,
  network, resources, history, startup/shutdown, PID/tmpfs resources, and
  debugging; `runtime/` owns generic supervisor timing, node count/storage
  admission budgets, and the kernel-wide sandbox Worker capacity; `execution/`,
  `service/`, `services/`, and `job/` own generic grouping, reconciliation, and
  job policy; `database/` owns node-local backend, location, credentials,
  connection-pool, and result-limit settings required before shared state can be
  loaded.
- `terminal/` owns retained-PTY idle expiry and `development/` owns idle
  development-sandbox shutdown. Both are node-local, runtime mutable durations
  with a one-millisecond minimum, applied by their existing kernel owners.
- No definition subtree or setting may describe an application protocol,
  application program, application state schema, or UUI behavior.

# Local Contracts

- Every `.toml` file defines exactly one setting and is recursively discovered
  by `kernel/cbus/gen`.
- Every file declares `storage = "node"` or `storage = "global"`; storage never
  falls back implicitly. String-format constraints use declarative `pattern`
  metadata rather than owner-specific settings parsing.
- Keys and environment variables are unique; every setting environment variable
  starts with `THE8020_`, and values must pass generated-catalog validation.
- Files are data, not executable hooks.
- `network.https_port` defaults to 8443 and requires restart; deployment images
  may provision their own port; zero disables native TLS on this node.
  `network.additional_http_port` defaults to zero (disabled) and is
  runtime mutable. Deployment images use it to retain their public HTTP mappings
  while keeping internal main-port connections. Certificate policy remains
  package-owned.
- All five `job.default.*` global defaults and `network.root_alias` are runtime
  mutable through their owners. Node-local additional HTTP uses bind-before-save
  replacement, including zero to disable it.
- `runtime.sandbox.keep_alive` is a node-local, restart-required integer in
  milliseconds, default `120000`; zero disables empty-sandbox retention. It
  applies to service and job/module sandboxes through the sandbox manager.
- `execution.grouping.job` defaults to `shared` for jobs, modules, and programs.
  Explicit placement labels and persisted strategy overrides remain effective.
- Logging has seven node-local runtime settings: enabled, minimum level,
  unified/source split, UTC period, maximum segment size, global byte cap, and
  maximum segment age. Batching/record/queue/sync limits are implementation
  constants. There are no HTTP-specific logging settings or per-identity files.

## Restart boundaries

The remaining restart-required definitions are deliberately retained below.
Their active values continue to describe the constructed runtime until restart.

| Settings | Current restart boundary |
| --- | --- |
| `database.backend`, `database.location`, `database.username`, `database.password` | Replace the authoritative database connection and all shared stores; outstanding transactions and publication must finish against the same database. |
| `execution.grouping.job`, `execution.grouping.service` | Captured by coordinator policies and existing group/allocation identity. A live change needs coordinated future-allocation semantics across both workloads. |
| `network.https_port` | Captured by HTTPS publication, independently of package enablement/certificate revisions. A separate port swap would be overwritten by the next package publication; both owners need one publication transaction. |
| `runtime.supervisor.heartbeat_interval`, `runtime.supervisor.heartbeat_timeout` | Interval is injected into each sandbox supervisor; timeout is captured by health monitoring. Updating only one side could falsely fail healthy running work. |
| `runtime.node.maximum_sandboxes`, `runtime.node.maximum_workers`, `runtime.node.temporary_storage_budget`, `runtime.sandbox.maximum_workers` | Copied into sandbox admission, Worker admission, coordinator packing, topology capacity and logging producer limits. They need coordinated reservations and over-capacity semantics before live reductions are safe. |
| `runtime.sandbox.keep_alive` | Captured by sandbox cleanup and allocation release. Unlike job Worker timers, the existing sandbox owner has no synchronized policy replacement covering release and cleanup. |
| `runtime.sandbox.startup_timeout`, `runtime.sandbox.stop_grace_period`, `runtime.worker.stop_grace_period` | Captured across backend construction, sandbox lifecycle specs and supervisor environments. Running and future native operations must agree on their deadlines. |
| `sandbox.containerd.socket`, `sandbox.enabled`, `sandbox.runtime.mode`, `sandbox.runtime.name` | Select and construct the isolation backend, diagnostics and development runtime. Replacing or disabling it would strand live sandbox ownership. |
| `sandbox.image.reference`, `sandbox.image.digest` | Verified image identity enters immutable runtime profiles, compatibility hashes and warm templates. Requires coordinated profile/generation replacement. |
| `sandbox.network.bridge`, `sandbox.network.name`, `sandbox.network.subnet` | Construct shared CNI/firewall state and sandbox network identities. Replacing them in place could disconnect running work. |
| `sandbox.network.egress_enabled` | No current production reader; workload profiles explicitly enable online access. Marking this definition live would falsely advertise enforcement. |
| `sandbox.resources.job.pid_maximum`, `sandbox.resources.job.tmpfs_maximum`, `sandbox.resources.service.pid_maximum`, `sandbox.resources.service.tmpfs_maximum` | Enter immutable profiles, resource hashes, mount sizes and warm templates. Live reductions cannot safely resize existing sandboxes. |
| `sandbox.debug.enabled`, `sandbox.debug.bind_address`, `sandbox.debug.lease_timeout` | Debug owner captures admission, loopback binding and lease policy at construction. Updating it requires synchronized handling of existing inspector leases. |
| `sandbox.history.retention` | Captured by archive creation and hourly cleanup; decreasing it deletes diagnostic history irreversibly. |
| `sandbox.startup_policy`, `sandbox.shutdown_policy` | Startup recovery already ran; shutdown cleanup captures its policy when runtime ownership is constructed. Changing either requires an explicit lifecycle policy contract. |
| `sandbox.warm_pool.size` | Per-profile resize exists, but concurrent clean assignment and trim need coordinated reservation before a global live reduction can guarantee it never deletes newly assigned work. |
| `services.reconcile_interval`, `services.startup_timeout` | Copied into service maintenance timers and version startup contexts; the current owner has no synchronized timer/deadline replacement. |

# Work Guidance

- Add no setting without a current kernel owner and acceptance requirement.

# Verification

- Generator and settings tests validate discovery, duplicates, types, mandatory
  storage, defaults, constraints, and runtime metadata.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
