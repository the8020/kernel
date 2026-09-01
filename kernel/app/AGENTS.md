# Purpose

- Compose and coordinate the Phase 1 kernel process.

# Ownership

- Own startup/shutdown order, generic settings arguments,
  bootstrap-authentication composition and cleanup lifecycle,
  full-versus-rootless runtime selection, degraded diagnostics,
  mapped package/state-store composition, service-package mounts, and
  filesystem-service reconciliation, plus development-workspace composition and
  shutdown, generic sandbox-console broker and SSH-server composition, and shared node
  topology/capacity-forwarding composition.
- Do not own command behavior, domain validation, transport parsing, or
  generated catalogs.

# Local Contracts

- Public API: `Config`, `RegisterHandlers`, `Main`, `Run`, and
  `ErrRestartRequested`.
- Dependencies are the generated definitions/registration callback plus the
  typed Phase 1A and Phase 1B package owners.
- The node root defaults to the current directory. An absent layout starts the
  guided path-selection procedure, while `--init-defaults` selects default
  mapped roots noninteractively. Initialization creates directories, validates
  Unix permissions, and records the mapping; it never installs defaults,
  packages, tools, or images. `--init-only` exits after node state creation.
- `--init-only --synchronize-packages` acquires the node lock and dispatches the
  generated `package.synchronize` handler in-process without starting listeners
  or runtime services. Installation uses this offline command-bus path when no
  kernel owns the instance.
- Startup order is load the explicit shared-root mapping → initialize shared
  and node-local state roots → lock → settings → logging →
  bootstrap-authentication store and per-node cleanup → package and
  desired-state stores → registry and
  development manager → network → authenticated console route → SSH listener → appliers →
  generated handler registration → command socket → asynchronous runtime-image
  record validation and runtime diagnostics/composition → initial terminal
  sandbox-history cleanup →
  configured fast inherited-sandbox destruction or explicit reconciliation →
  workload-record cleanup → one-time filesystem-service discovery plus
  active-runtime-only maintenance → heartbeat/OOM and hourly history-retention
  monitoring.
- The command socket publishes `runtime initialization is in progress` until one
  complete runtime dependency snapshot is ready; runtime commands fail safely
  during that interval while system/settings/authentication administration
  remains available.
- `sandbox.startup_policy` defaults to `destroy`, which enumerates
  instance-owned metadata and force-deletes inherited backend objects without
  task, network, or supervisor health probes. `reconcile` is the explicit
  cross-restart preservation mode.
- `sandbox.warm_pool.size` defaults to zero. Jobs and enabled
  filesystem services create sandboxes from explicit command or request demand;
  configured positive warm capacity remains available as an opt-in latency
  tradeoff.
- `sandbox.history.retention` is node-local, restart-required, and defaults to
  seven days. Runtime composition supplies mode-specific live log paths to the
  separate history store and performs cleanup without adding history to live
  sandbox scans; cleanup errors are logged without making live runtime
  composition unavailable.
- Restart restoration never rebinds a listener for an unavailable sandbox;
  debug listeners are always discarded because their token and Go handler are
  memory-only.
- Persisted live service pools whose sandbox is absent from the reconciled
  healthy set are retired locally before service restoration, including pools
  already left `FAILED` by an earlier run; startup never waits on supervisor
  calls to known-missing groups.
- Workload-record quarantine, failure propagation, service/job restoration, and
  host-port restoration are best-effort per workload. Their errors are logged
  and isolated; only failure of a shared runtime prerequisite may make runtime
  composition unavailable.
- Runtime-host failures retain both full and rootless diagnostics and
  command-bus availability without a native-Deno fallback; `auto` prefers full
  mode and selects rootless only when full host authority is unavailable.
- Development workspaces select direct runsc consistently with the configured
  full/rootless runtime mode. They use their separate development-image
  materialization and state roots and remain administrable independently of
  asynchronous service-runtime initialization.
- Process composition creates one registry before the development manager,
  supplies it to the workspace-scoped activation gateway, then registers the
  generated handlers before any administrative command can create a workspace.
  The shared `config/development-mounts.toml` profile is the production mount
  input; its absence falls back to the equivalent built-in profile.
- Development-manager initialization starts inherited development-sandbox
  deletion asynchronously without restoring process state or scanning all
  workspace records; native durable source and system roots remain
  available for explicit workspace start.
- The console broker registers `/_the8020/console` on the loopback main
  listener after authentication and development composition, receives the
  ordinary runtime sandbox manager only after asynchronous runtime startup,
  tracks browser and SSH PTY leases, closes runtime sessions when that provider
  is withdrawn, and closes all PTYs during kernel shutdown.
- SSH composition reads restart-required `network.ssh_port`, uses the private
  `node/kernel/ssh/host_ed25519` key, and starts only after authentication,
  development lifecycle, and the shared console broker are available.
- Configured image reference and optional immutable digest must match the
  selected pinned runtime before sandbox composition proceeds; the configured
  containerd runtime name applies only to full mode.
- Runtime composition supplies an instance-root-bounded mount policy for explicit
  development workspaces; mounting the instance root itself is rejected because
  it would expose protected `node/kernel` data.
- Runtime composition supplies the already registered command bus to the
  authenticated supervisor callback; it does not construct a second
  administrative registry.
- Service runtime profiles mount the configured package root read-only at
  `/workspace/packages` and shared package state read-write at
  `/state/package-data`, grant Workers read-only access to the bundled generic
  `/opt/runtime` modules, and keep portable dependency mode in runtime-group
  compatibility. The kernel never scans or interprets package state.
- Runtime composition derives node memory/CPU/temp-storage budgets when their
  node-local setting is zero, applies node Worker admission plus kernel-wide
  per-sandbox Worker/CPU/RAM targets, derives the canonical service framework
  defaults, and publishes local sandbox/Worker/execution-slot capacity to the
  authenticated node topology owner.
- Runtime composition exposes generic exact-Worker invocation through the
  authenticated local/cross-node path and generic persistent-execution
  completion through the supervisor callback. Function names and JSON payloads
  remain opaque to composition.
- `config/nodes.toml` and `.nodes.key` initialize before runtime composition.
  The configured local recipient listener starts only after the service router
  and capacity provider exist and forwards both HTTP and WebSocket traffic.
- Main-listener composition reads the restart-required global
  `network.root_alias` value and supplies it to the network owner before the
  listener starts.
- Graceful shutdown has nine completed-stage progress units rather than an
  elapsed-time estimate: public HTTP, runtime initialization join, runtime
  controllers, runtime ports, runtime sandboxes, runtime backends,
  authentication maintenance, administrative socket, and process resources.
- Shutdown first drains command intake while retaining `system status` and
  idempotent `system shutdown` and `system restart`. SSH and console sessions
  close before sandbox cleanup. Public HTTP draining, authentication cleanup,
  and runtime cancellation/join overlap; filesystem-service,
  execution-service, job, and warm-pool controllers stop concurrently after
  monitoring stops; ports then close before sandboxes; callback and sandbox
  backend endpoints close concurrently after sandbox cleanup. The administrative
  socket closes late, followed by logging and the instance lock.
- After a restart request completes the same cleanup and releases the instance
  lock, `Main` exec-replaces the current process from its invoked executable
  path with the original arguments and environment. This preserves the PID,
  loads a newly materialized binary, and does not depend on a parent wrapper.
- Development sandbox process destruction runs during kernel shutdown before
  logging and instance-lock release; durable workspace files require no flush.
- Filesystem-service maintenance never polls the complete package catalog.
  Startup discovers once; explicit service actions and cold requests reconcile
  directly, while the timer touches only live or capacity-pending services.
- Extend composition only when a kernel-owned Phase requirement adds a real
  lifecycle service.

# Work Guidance

- Keep imports from the `.development/generated` build module out of authored code by
  accepting generated catalogs/settings in `Config`; kernel runtime-protocol
  consumers use the generator-maintained mirror under `kernel/runtime/protocol`.
  Process composition must never depend on the platform source tree or execute
  runtime/image construction scripts.

# Verification

- `control_plane_test.go` proves the command socket remains usable while runtime
  initialization is deliberately blocked, observes the atomic transition to
  ready, then verifies live shutdown status and mutation rejection during
  cleanup. `integration_test.go` covers socket readiness, status, both admin
  modes, complete interactive help, compact/detailed settings lists, precedence,
  live network/logging changes, root alias redirection and validation, failure
  rollback, separate node/global persistence through the same commands,
  persistence across restart, unset, shutdown/restart instructions, and cleanup;
  `runtime_test.go` covers startup failure propagation, healthy-sandbox
  selection, ordered cleanup stages, and concurrent controller cleanup.

# Child DOX Index
