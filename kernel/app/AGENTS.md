# Purpose

- Compose and coordinate the Phase 1 kernel process.

# Ownership

- Own startup/shutdown order, generic settings arguments,
  system-database pool composition and catalog-gated service readiness,
  database-backed authentication composition and cleanup lifecycle,
  full-versus-rootless runtime selection, degraded diagnostics,
  package/database-store composition, service-package mounts, and service
  reconciliation, plus development-sandbox composition and
  shutdown, generic sandbox-console broker and SSH-server composition, and shared node
  topology/capacity-forwarding composition.
- Do not own command behavior, domain validation, transport parsing, or
  generated catalogs.

# Local Contracts

- Public API: `Config`, `RegisterHandlers`, `Main`, `Run`, and
  `ErrRestartRequested`.
- Dependencies are the generated definitions/registration callback plus the
  typed Phase 1A and Phase 1B package owners.
- The node root defaults to the current directory. Initialization creates the
  fixed package/user/node/database layout, validates Unix permissions, and
  records node identity and node-local settings in `kernel.toml`; it never
  installs packages, tools, or images. `--init-only` exits after node creation.
- Startup order is load the fixed layout → lock → node settings → logging →
  built-in command registry/socket → asynchronous database connection and internal
  catalog initialization → database-backed global settings, authentication,
  secrets, topology, packages and services →
  development manager → network → authenticated console route → SSH listener → appliers →
  runtime-image
  record validation and runtime diagnostics/composition → initial terminal
  sandbox-history cleanup →
  configured fast inherited-sandbox destruction or explicit reconciliation →
  service-record cleanup → initialize/validate the database catalog → compose
  the non-durable job runtime, exact-package program runner, and table evaluator
  → recover a pending schema deployment or
  fully synchronize an uninitialized database → atomically index package CBus
  commands → one-time installed-service discovery plus
  active-runtime-only maintenance → heartbeat/OOM and hourly history-retention
  monitoring.
- The command socket publishes `runtime initialization is in progress` until one
  complete runtime dependency snapshot is ready; runtime commands fail safely
  during that interval while `kernel.*` recovery and lifecycle administration
  remains available.
- Database connection, catalog, or first full-table synchronization failure is
  logged and cached in status. It never prevents the administrative socket or
  built-in `kernel.config.*` and package recovery commands from running, but it
  prevents package commands, ordinary services, and UUI from starting.
- Authentication composition verifies its package-owned users and session
  tables before the service plane is published; an empty users table is valid,
  while an inaccessible table is a runtime initialization failure.
- A fresh database synchronizes every installed package table in bounded
  evaluator batches and becomes `READY` only after all schemas and package
  activation hooks succeed. A normal boot trusts the initialized marker and
  does not scan definitions or repeat completed hooks.
- Database pool limits are node-local runtime settings applied transactionally
  to the already running pool; backend, location, and credentials remain node-local
  restart settings.
- `sandbox.startup_policy` defaults to `destroy`, which enumerates
  instance-owned metadata and force-deletes inherited backend objects without
  task, network, or supervisor health probes. `reconcile` is the explicit
  cross-restart preservation mode.
- `sandbox.warm_pool.size` defaults to zero. Jobs and enabled
  services create sandboxes from explicit command or request demand;
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
- Ordinary jobs are memory-only and have no startup restoration phase.
- Persisted live service pools whose sandbox is absent from the reconciled
  healthy set are retired locally before service restoration, including pools
  already left `FAILED` by an earlier run; startup never waits on supervisor
  calls to known-missing groups.
- Service-record quarantine, failure propagation, and host-port restoration are
  best-effort per workload. Their errors are logged
  and isolated; only failure of a shared runtime prerequisite may make runtime
  composition unavailable.
- Runtime-host failures retain both full and rootless diagnostics and
  command-bus availability without a native-Deno fallback; `auto` prefers full
  mode and selects rootless only when full host authority is unavailable.
- Development sandboxes select direct runsc consistently with the configured
  full/rootless runtime mode. They use their separate development-image
  materialization and node-local runtime roots and remain administrable independently of
  asynchronous service-runtime initialization.
- Process composition creates one registry before the development manager,
  supplies it to the sandbox activation gateway, then registers the
  generated handlers before any administrative command can create a sandbox.
  The small built-in development mount profile is canonical.
- Development-manager initialization starts inherited development-sandbox
  deletion asynchronously without restoring process state or scanning all
  sandbox records; durable overlay and system state remain available for an
  explicit user sandbox start.
- The console broker registers `/_the8020/console` on the loopback main
  listener after authentication and development composition, receives the
  ordinary runtime sandbox manager only after asynchronous runtime startup,
  tracks browser and SSH PTY leases, closes runtime sessions when that provider
  is withdrawn, and closes all PTYs during kernel shutdown.
- SSH composition reads runtime-mutable `network.ssh_port`, registers the SSH
  manager as its transactional runtime applier, uses the private
  `node/kernel/ssh/host_ed25519` key, and starts only after authentication,
  development lifecycle, and the shared console broker are available.
- Configured image reference and optional immutable digest must match the
  selected pinned runtime before sandbox composition proceeds; the configured
  containerd runtime name applies only to full mode.
- Runtime composition supplies an instance-root-bounded mount policy for explicit
  job/Worker workspaces; mounting the instance root itself is rejected because
  it would expose protected `node/kernel` data.
- Runtime composition supplies the already registered command bus to the
  authenticated supervisor callback and publishes runtime operations plus the
  package-command indexer after the job/program path is ready; it does not
  construct a second administrative registry.
- Runtime composition supplies the kernel-owned database service to the
  authenticated supervisor callback. Neither the supervisor nor a Worker
  receives database credentials or direct database network permissions.
- Package composition injects only the secret store's narrow value resolver
  into the package manager; command services separately expose authenticated
  secret administration. Deno and application packages never receive the
  secret storage internals.
- Service and job runtime profiles mount the activated package root read-only at
  `/workspace/packages` and the runtime callback directory at `/run/the8020`,
  grant Workers read-only access to bundled `/opt/runtime` modules, unrestricted
  outbound network/imports, and writable `/tmp` and `/runtime-cache`, and keep
  portable dependency mode in runtime-group compatibility. Durable shared
  application data goes through the kernel database bridge.
- Runtime composition derives the node temporary-storage budget when its
  node-local setting is zero, applies node Worker admission and the kernel-wide
  per-sandbox Worker maximum, derives the canonical service framework defaults,
  and publishes local sandbox/Worker/execution-slot capacity to the authenticated
  node topology owner. CPU and RAM are not admission dimensions.
- Runtime composition exposes generic exact-Worker invocation through the
  authenticated local/cross-node path and generic persistent-execution
  completion through the supervisor callback. Function names and JSON payloads
  remain opaque to composition.
- Database-backed topology initializes before runtime composition. The
  configured local recipient listener starts only after the service router
  and capacity provider exist and forwards both HTTP and WebSocket traffic.
- Main-listener composition reads the restart-required global
  `network.root_alias` value and supplies it to the network owner before the
  listener starts.
- Graceful shutdown has nine completed-stage progress units rather than an
  elapsed-time estimate: public HTTP, runtime initialization join, runtime
  controllers, runtime ports, runtime sandboxes, runtime backends,
  authentication maintenance, administrative socket, and process resources.
- Shutdown first drains command intake while retaining `kernel.status` and
  idempotent `kernel.shutdown` and `kernel.restart`. SSH and console sessions
  close before sandbox cleanup. Public HTTP draining, authentication cleanup,
  and runtime cancellation/join overlap; package-service,
  execution-service, job, and warm-pool controllers stop concurrently after
  monitoring stops; ports then close before sandboxes; callback and sandbox
  backend endpoints close concurrently after sandbox cleanup. The administrative
  socket closes late, followed by logging and the instance lock.
- After a restart request completes the same cleanup and releases the instance
  lock, `Main` exec-replaces the current process from its invoked executable
  path with the original arguments and environment. This preserves the PID,
  loads a newly materialized binary, and does not depend on a parent wrapper.
- Development manager shutdown checkpoints private package deltas before it
  destroys owned sandbox processes, then completes before logging and
  instance-lock release.
- Service maintenance never polls the complete package catalog.
  Startup discovers once; explicit service actions and cold requests reconcile
  directly, while the timer touches only live or capacity-pending services.
  The shared-state monitor scalar-polls package and direct-service revisions;
  only an advanced revision loads and reconciles its affected IDs. Database or
  package-source convergence failures gate the public plane; a failure to start
  one affected service remains local, keeps its revision pending, and retries
  without taking unrelated services offline.
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
  live HTTP/SSH listener and logging changes, root alias redirection and
  validation, occupied-port rollback, separate node/global persistence through
  the same commands, persistence across restart, unset, shutdown/restart
  instructions, and cleanup;
  `runtime_test.go` covers startup failure propagation, healthy-sandbox
  selection, ordered cleanup stages, and concurrent controller cleanup.

# Child DOX Index
