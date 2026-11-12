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
- `network.https_port` defaults to 8443 and is runtime mutable; deployment images
  may provision their own port; zero disables native TLS on this node.
  `network.additional_http_port` defaults to zero (disabled) and is
  runtime mutable. Deployment images use it to retain their public HTTP mappings
  while keeping internal main-port connections. Certificate policy remains
  package-owned.
- All five `job.default.*` global defaults and `network.root_alias` are runtime
  mutable through their owners. Node-local additional HTTP uses bind-before-save
  replacement, including zero to disable it.
- `runtime.sandbox.keep_alive` is a node-local, runtime-mutable integer in
  milliseconds, default `120000`; zero disables empty-sandbox retention. It
  applies to service and job/module sandboxes through the sandbox manager.
- `execution.grouping.job` defaults to `shared` for jobs, modules, and programs.
  Explicit placement labels and persisted strategy overrides remain effective.
- Logging has seven node-local runtime settings: enabled, minimum level,
  unified/source split, UTC period, maximum segment size, global byte cap, and
  maximum segment age. Batching/record/queue/sync limits are implementation
  constants. There are no HTTP-specific logging settings or per-identity files.

## Runtime application

- Node-local settings use ordinary `kernel.config.set`/`unset`. The runtime
  applier prepares a complete validated policy before persistence; failed
  validation or persistence changes no owner. Commits update synchronized owners
  without creating or restarting work.
- `runtime.node.maximum_sandboxes`, `runtime.node.maximum_workers`,
  `runtime.node.temporary_storage_budget`, and `runtime.sandbox.maximum_workers`
  govern new admission. Reductions retain admitted sandboxes, Workers and
  reservations, even above the new limit; topology advertises current limits.
- `execution.grouping.job` selects future job allocations. Submitted/queued jobs capture their profile, grouping and resource
  policy; existing service pools retain their sandboxes and exact routes.
- The four `sandbox.resources.{job,service}.{pid_maximum,tmpfs_maximum}`
  settings update future profiles, resource hashes, tmpfs mounts and warm
  templates. Existing instances retain immutable mounts/resources; PID cgroup
  enforcement remains unavailable in rootless mode.
- `sandbox.warm_pool.size` reconciles asynchronously. Obsolete clean capacity
  trims through atomic reservation; assigned sandboxes never return to the clean
  pool. New resources replace clean templates without killing assigned work.
- `runtime.sandbox.keep_alive` uses the current duration against existing idle
  timestamps on the next bounded maintenance pass. `sandbox.history.retention`
  affects new archive expiry annotations and the next hourly whole-bucket cleanup
  of existing history; reducing it can irreversibly delete diagnostic metadata.
  Old archive expiry annotations remain creation-time observations. Logs retain
  their independent logd policy.
- `runtime.sandbox.startup_timeout`, `runtime.sandbox.stop_grace_period`, and
  `services.startup_timeout` affect subsequent operations; an operation already
  underway keeps its deadline. `services.reconcile_interval` resets the existing
  maintenance timer. `sandbox.shutdown_policy` selects the next shutdown action.
- `runtime.supervisor.heartbeat_interval` and
  `runtime.worker.stop_grace_period` reach supervisors through authenticated
  registration/heartbeat acknowledgements. Supervisor snapshots acknowledge the
  applied cadence; heartbeat timeout reductions allow the old observed cadence
  until acknowledgement, avoiding false failure during propagation.
  `runtime.supervisor.heartbeat_timeout` governs subsequent health checks.
- `sandbox.debug.enabled`, `sandbox.debug.bind_address`, and
  `sandbox.debug.lease_timeout` govern new leases. Issued token leases retain
  their listener and expiry; disabling admission does not revoke them.
- `network.https_port` shares the native HTTPS publication lock with package
  certificate/enablement updates, prepares the replacement before saving, and
  preserves established connections. Zero disables the listener; changing the
  port does not enable package HTTPS policy.

- `database.password` authenticates the candidate on a separate, five-second-bounded
  connection to the active PostgreSQL database and username before saving. Commit
  changes credentials for future physical connections; existing pooled connections
  and in-flight transactions retain their identity. Failed authentication or
  persistence preserves active credentials; unset validates the revealed fallback
  in the same way. SQLite ignores the password. Database-side password changes are
  an operator action; update this node-local setting on each affected node after
  provisioning the password. Pending backend/location/username changes do not alter
  the validation target. Native credential repair is available before application
  initialization, including when the initial login fails.

## Restart boundaries

These thirteen settings retain restart metadata. None is excluded merely for
lacking a setter. The operational transitions below cross durable identity,
installed isolation artifacts, or infrastructure ownership.

| Settings | Verified constraint and required transition |
| --- | --- |
| `database.backend`, `database.location`, `database.username` | `app.Run` and `database.Manager` establish one authoritative pool used by settings, topology, package stores and deployment locks. A location/backend switch can select a different system identity/schema while old transactions still publish to the old store. A username switch changes SQL grants, row-level security and object ownership while existing transactions retain the original authority. These transitions require quiescing transactions, validating the target database/authority and restarting against it; cross-database migration is outside settings mutation. Password-only rotation does not cross this boundary. |
| `sandbox.containerd.socket`, `sandbox.runtime.mode`, `sandbox.runtime.name`, `sandbox.enabled` | `app.initializeRuntime` selects the isolation authority and composes shared managers, callbacks, development execution and inherited cleanup. Moving to another daemon/backend or disabling that owner while its tasks run loses the authority needed for stop/reap/reconcile. Drain and clean up the old authority before restart; enabling from disabled requires the normal complete runtime-readiness path. Admission limits remain independently live. |
| `sandbox.image.reference`, `sandbox.image.digest` | These are assertions about the installer-pinned image, not an arbitrary image-selection API. `app.validateConfiguredRuntimeIdentity` rejects a name/digest inconsistent with the verified manifest/image record. Rootless OCI roots share the canonical installed rootfs (`rootless.Backend.ociSpec`); full mode resolves the verified imported digest. Installing another image changes the runtime source/protocol and verified smoke artifacts, so use the ordinary staged kernel installer/updater and its restart. Merely changing either setting cannot materialize or qualify an image. Future PID/tmpfs defaults are independently live. |
| `sandbox.network.bridge`, `sandbox.network.name`, `sandbox.network.subnet` | CNI `network.Manager` uses the same conflist for ADD/CHECK/DEL; allocation records do not retain a separate conflist generation. nftables derives bridge/subnet routing from that same topology. Replacing it under live namespaces can misroute traffic or make DEL release the wrong network/IPAM allocation. Drain/delete old namespaces under their original CNI policy, provision the replacement network and restart; parallel network migration is an infrastructure change. |
| `sandbox.startup_policy` | Startup recovery has already destroyed or reconciled inherited tasks before live admission. Reapplying it would destroy current work or mistake it for inherited work. It is persisted for the next startup only, never replayed against the current runtime. |

## Superseded service grouping

`execution.grouping.service` is bypassed by every ordinary service allocation:
`execution.services.Manager.Start` supplies the package-owned sandbox group,
including the explicit empty shared group, and `placement.selectKey` gives that
value precedence. Changing it or restarting cannot affect those allocations.
Its definition now rejects mutations rather than advertising a live or restart
change. Configure service placement through the services package. Future service
resource defaults remain live and existing allocations keep their group.

## Unresolved egress setting

`sandbox.network.egress_enabled` had no production reader and still does not
enforce policy. `app.initializeRuntime` explicitly grants ordinary workloads
online imports and outbound networking; full-mode nftables consumes each
sandbox's profile, and rootless host networking has no firewall boundary.
Activating the legacy default `false` would silently revoke the established
ordinary runtime contract, while changing metadata alone would falsely claim
security enforcement. Its definition now rejects runtime mutations and removes
the false restart-enforcement claim. Existing startup overrides remain readable
for diagnosis but are unenforced. Actual network denial requires explicit
profile/permission policy plus full-mode firewall enforcement (and a qualified
rootless isolation alternative); a restart does not supply this missing policy.

# Work Guidance

- Add no setting without a current kernel owner and acceptance requirement.

# Verification

- Generator and settings tests validate discovery, duplicates, types, mandatory
  storage, defaults, constraints, and runtime metadata.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
