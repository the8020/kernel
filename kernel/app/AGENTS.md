Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Compose and coordinate the Phase 1 kernel process.

# Ownership

- Own startup/shutdown order, generic settings arguments, system-database pool
  composition and catalog-gated service readiness, private deployment-key and
  users-package hook composition, full-versus-rootless runtime selection,
  degraded diagnostics, package/database-store composition, service-package
  mounts, and service reconciliation, plus development-sandbox composition and
  shutdown, generic sandbox-console broker and SSH-server composition, and
  shared node topology/capacity-forwarding composition.
- Do not own command behavior, domain validation, transport parsing, or
  generated catalogs.

# Local Contracts

- `sandbox_access.go` connects authenticated native sandbox ingress to the
  ordinary users authenticate program and service router. Allowance issuance
  executes as the verified sandbox owner; service requests require a signed
  token for that owner and retain target-Worker account/session validation.
  Native provenance follows the existing authenticated peer transport; payloads
  remain bounded, and credentials never enter diagnostics.

- Public API: `Config`, `RegisterHandlers`, `Main`, `Run`, and
  `ErrRestartRequested`.
- Dependencies are the generated definitions/registration callback plus the
  typed Phase 1A and Phase 1B package owners.
- The node root defaults to the current directory. Initialization creates the
  fixed package/user/node/database layout, validates Unix permissions, and
  records node identity and node-local settings in `kernel.toml`; it never
  installs packages, tools, or images. `--init-only` exits after node creation.
- Startup order is load the fixed layout → lock → private signing key → node
  settings → asynchronous logd/producer and node logging applier → built-in
  command registry/socket → asynchronous database connection and internal
  catalog initialization → database-backed global settings, secrets, topology,
  packages and services → development manager → network → authenticated console
  route → SSH listener → appliers → runtime-image record validation and runtime
  diagnostics/composition → initial terminal sandbox-history cleanup →
  configured fast inherited-sandbox destruction or explicit reconciliation →
  service-record cleanup → initialize/validate the database catalog → compose
  the non-durable job runtime, shared-package program runner, and table
  evaluator → recover a pending schema deployment or fully synchronize an
  uninitialized database → index package commands, events, and hooks → run one
  ordinary service-index hook job and publish package fragments plus
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
- Before database startup, load or generate the signing key under
  `node/kernel/keys/signing.key`. Publish its primitive directly to the existing
  command bus; status and replacement remain available during database failure.
  Composition selects `/p/the8020/users/mod.ts` for protected target-Worker
  hooks. Native SSH/browser-console adapters run `the8020/users/authenticate`
  through the ordinary system-user program/job path, with normal mounts and
  secure inputs. No auth-specific service, runtime, registry, or maintenance
  timer exists.
- A fresh database synchronizes every installed package table in bounded
  evaluator batches and becomes `READY` only after all schemas and package
  activation hooks succeed. A normal boot trusts the initialized marker and does
  not scan definitions or repeat completed hooks.
- Database pool limits are node-local runtime settings applied transactionally
  to the already running pool; backend, location, and credentials remain
  node-local restart settings.
- The companion logd starts independently of runtime/database readiness. Kernel
  stdout/stderr capture is enabled by `Main`; embedded `Run` callers do not
  replace their host process descriptors. Tests supply a real companion binary.
  Register all seven logging settings before administration starts, then let
  global settings attach without replacing node logging policy.
- Kernel boot/version, runtime readiness, and final exit events use the bounded
  producer. logd has no parent-death kill signal and drains direct raw pipes on
  kernel crash. Logging status and `kernel.logs` do not require a healthy
  runtime or database.
- Logging reopens inherited sandbox raw endpoints before asynchronous database
  startup. Runtime lifecycle later authenticates their existing supervisor
  tokens or removes the endpoints after confirmed native cleanup.
- `sandbox.startup_policy` defaults to `destroy`, which enumerates
  instance-owned metadata and force-deletes inherited backend objects without
  task, network, or supervisor health probes. `reconcile` is the explicit
  cross-restart preservation mode.
- `sandbox.warm_pool.size` defaults to zero. Jobs and enabled services create
  sandboxes from explicit command or request demand; configured positive warm
  capacity remains available as an opt-in latency tradeoff.
- `sandbox.history.retention` is node-local, restart-required, and defaults to
  seven days. Runtime composition supplies a separate metadata history root and
  performs cleanup without adding history to live sandbox scans. Log retention
  remains independently owned by logd; history cleanup errors are logged without
  making live runtime composition unavailable.
- Restart restoration never rebinds a listener for an unavailable sandbox; debug
  listeners are always discarded because their token and Go handler are
  memory-only.
- Ordinary jobs are memory-only and have no startup restoration phase.
- Job composition supplies the persistent node ID and the logging manager's
  cached reader-position getter; taking execution log references requires no
  additional socket call.
- Node composition registers the existing log reader before starting the
  authenticated recipient listener. Remote log reads use the same bounded
  logging API; local administrative reads remain available before that listener.
- The event dispatcher starts its minute-aligned timer only after the complete
  runtime dependency snapshot is published. One `runtimeIndexer.Reindex` entry
  point refreshes commands and both handler indexes and package-scoped service
  fragments at startup, after local activation, after shared source publication,
  and through `kernel.reindex`. A nil/empty package selection is a full rebuild;
  lifecycle callers pass only changed package IDs. A successful revision refresh
  is retained across service retries so those retries never repeat discovery.
  The activation owner validates event/hook declarations and their program
  references against candidate and ready package roots before publication.
  Dispatcher shutdown cancels and joins outstanding listeners alongside the
  ordinary job controller. Application scheduling and durable recovery remain in
  Deno packages. Invalid service fragments report publication errors while
  native commands and the ordinary job runtime remain available for normal
  services-package repair. Accepted fragments stay live on failure, and
  unrelated packages keep serving. Runtime startup/retirement errors after
  publication are reported as accepted fragments with runtime diagnostics; they
  never rerun provider jobs. The existing service maintenance queue owns
  capacity and retirement retries.
- Persisted live service pools whose sandbox is absent from the reconciled
  healthy set are retired locally before service restoration, including pools
  already left `FAILED` by an earlier run; startup never waits on supervisor
  calls to known-missing sandboxes.
- Service-record quarantine, failure propagation, and host-port restoration are
  best-effort per workload. Their errors are logged and isolated; only failure
  of a shared runtime prerequisite may make runtime composition unavailable.
- Runtime-host failures retain both full and rootless diagnostics and
  command-bus availability without a native-Deno fallback; `auto` prefers full
  mode and selects rootless only when full host authority is unavailable.
- Development sandboxes select direct runsc consistently with the configured
  full/rootless runtime mode. They use their separate development-image
  materialization and node-local runtime roots and remain administrable
  independently of asynchronous service-runtime initialization.
- Process composition creates one registry before the development manager,
  supplies it to the sandbox activation gateway, then registers the generated
  handlers before any administrative command can create a sandbox. The small
  built-in development mount profile is canonical.
- Development composition supplies a loopback system-URL getter from the active
  `network.main_port`; each sandbox start receives the current value without
  embedding instance configuration in package-owned guidance.
- Development-manager initialization starts inherited development-sandbox
  deletion asynchronously without restoring process state or scanning all
  sandbox records; durable overlay and system state remain available for an
  explicit user sandbox start.
- The console broker registers `/_the8020/console` on the loopback main listener
  after authentication and development composition, receives the ordinary
  runtime sandbox manager only after asynchronous runtime startup, tracks
  browser and SSH PTY leases, closes runtime sessions when that provider is
  withdrawn, and closes all PTYs during kernel shutdown.
- The same broker is published in the platform snapshot for Deno terminal
  operations. Runtime callback composition connects Worker resource release to
  attachment cleanup; it neither creates another broker nor owns display state.
  The node manager receives the same operation dispatcher's physical terminal
  closer so authenticated exact-node cleanup survives display-Worker loss.
- Console composition reserves development lifetime through the development
  manager. It applies initial terminal/development idle settings and registers
  their runtime appliers before publishing console or SSH admission.
- SSH composition reads runtime-mutable `network.ssh_port`, registers the SSH
  manager as its transactional runtime applier, uses the private
  `node/kernel/ssh/host_ed25519` key, and starts only after authentication,
  development lifecycle, and the shared console broker are available.
- Named SSH admission calls the dev-core terminal service's `/open` through
  generic local service dispatch with the already approved native principal,
  then attaches the returned physical PTY through the same broker. Package code
  owns display state, labels, and metadata; Go owns no terminal renderer.
- Configured image reference and optional immutable digest must match the
  selected pinned runtime before sandbox composition proceeds; the configured
  containerd runtime name applies only to full mode.
- Runtime composition supplies an instance-root-bounded mount policy for
  explicit job/Worker workspaces; mounting the instance root itself is rejected
  because it would expose protected `node/kernel` data.
- Runtime composition supplies the already registered command bus to the
  authenticated supervisor callback and publishes runtime operations plus the
  shared package reindex entry point after the job/program path is ready; it
  does not construct a second administrative registry.
- Runtime composition supplies the kernel-owned database service to the
  authenticated supervisor callback. Neither the supervisor nor a Worker
  receives database credentials or direct database network permissions.
- Runtime composition gives the sandbox lifecycle owner the existing logging
  manager so authenticated log sources follow native creation and cleanup.
- Runtime composition supplies the node's sandbox keepalive, default two
  minutes. Existing health maintenance wakes at most one second apart and
  processes the cached idle queue as well as stale heartbeats; normal callbacks
  update Worker counts without polling supervisors. Explicit zero duration
  overrides remain zero. Development and reserved warm capacity retain their own
  lifetime rules.
- Full runtime readiness and task creation share the complete backend
  configuration. The initial probe client closes after its read; the retained
  doctor uses the live runtime backend once connected, never the retired probe
  client.
- Package composition injects only the secret store's narrow value resolver into
  the package manager; command services separately expose authenticated secret
  administration. Deno and application packages never receive the secret storage
  internals.
- Service defaults never override the normal runtime dependency profile. Online
  imports remain available to dynamic package dependencies, including request
  authentication, without warming an unrelated service or job first.
- Service and job runtime profiles mount the activated package root read-only at
  `/workspace/packages` and the runtime callback directory at `/run/the8020`,
  grant Workers read-only access to bundled `/opt/runtime` modules, unrestricted
  outbound network/imports, and writable `/tmp` and `/runtime-cache`, and keep
  portable dependency mode in sandbox compatibility. Durable shared application
  data goes through the kernel database bridge.
- Runtime composition creates
  `node/kernel/runtime/deno-cache/<Deno version>/<backend>/` once and
  bind-mounts its writable `npm`, `remote`, and `gen` directories into both
  workload profiles. The bounded `/runtime-cache` tmpfs retains private
  SQLite/WAL and other process caches. Entries survive sandbox destruction and
  kernel restart and remain visible to already-running sandboxes. Backend
  separation avoids mixing the rootless and image-user filesystem ownership.
  Only those three child directories are mounted; the enclosing node directory
  stays private. Deno owns file publication and source/version validation.
  Simultaneous cold misses may duplicate work; no cache-wide execution lock,
  cache scanning, or application replay is introduced. Development composition
  does not use this cache.
- Runtime composition derives the node temporary-storage budget when its
  node-local setting is zero, applies node Worker admission and the kernel-wide
  per-sandbox Worker maximum, and publishes local sandbox/Worker/execution-slot
  capacity to the authenticated node topology owner. CPU and RAM are not
  admission dimensions.
- Runtime composition exposes generic exact-Worker invocation through the
  authenticated local/cross-node path and generic persistent-execution
  completion in the owning supervisor, without a Go route registry or callback.
  Function names and JSON payloads remain opaque to composition.
- Database-backed topology initializes before runtime composition. The
  configured local recipient listener starts only after the service router and
  capacity provider exist and forwards both HTTP and WebSocket traffic.
- Main-listener composition reads the restart-required global
  `network.root_alias` value and supplies it to the network owner before the
  listener starts.
- Graceful shutdown has eight completed-stage progress units rather than an
  elapsed-time estimate: public HTTP, runtime initialization join, runtime
  controllers, runtime ports, runtime sandboxes, runtime backends,
  administrative socket and process resources.
- Shutdown first drains command intake while retaining `kernel.status`,
  `kernel.logs`, and idempotent `kernel.shutdown` and `kernel.restart`. SSH and
  console sessions close before sandbox cleanup. Public HTTP draining and
  runtime cancellation/join overlap; package-service, execution-service, job,
  and warm-pool controllers stop concurrently after monitoring stops; ports then
  close before sandboxes; callback and sandbox backend endpoints close
  concurrently after sandbox cleanup. The administrative socket closes late,
  followed by logging and the instance lock.
- After a restart request completes the same cleanup and releases the instance
  lock, `Main` exec-replaces the current process from its invoked executable
  path with the original arguments and environment. This preserves the PID,
  loads a newly materialized binary, and does not depend on a parent wrapper.
- Development manager shutdown checkpoints private package deltas before it
  destroys owned sandbox processes, then completes before logging and
  instance-lock release.
- Service maintenance never polls the complete package catalog. Startup indexes
  through Deno; explicit service actions and cold requests reconcile directly,
  while the timer touches only live or capacity-pending services. The
  shared-state monitor scalar-polls package and generic index revisions; only an
  advanced revision loads and reconciles its affected IDs. Database or package
  revision-read failures gate the public plane; a failure to start one affected
  service remains local, keeps its revision pending, and retries without taking
  unrelated services offline.
- Local activation and the shared-state monitor use the same revision consumer.
  A source update scans current service Worker imports and publishes
  deduplicated soft-restart intent before reindexing changed package fragments;
  the generic index follower then applies restart markers on every node.
  Scan/publication failures retain the update for retry without gating unrelated
  traffic. Composition injects Worker inspection and generic lifecycle methods;
  package code owns source-update orchestration. Followers never copy sources.
- The installed transaction overlay claims selected package IDs under short
  memory locks, performs indexing outside them, and retains later requests.
  Synchronous callers wait only for selected busy owners after releasing other
  claims. The monitor queues at most 16 bounded background batches; shutdown
  cancels and joins them. Its implementation and focused checks are in
  [development analysis](../development/analysis/AGENTS.md).
- The runtime monitor uses cheap scalar package/index revisions on its normal
  cadence. Shared node topology refresh is independently bounded and never
  becomes a per-request or one-second full-table dependency; runtime callbacks
  update sandbox observations directly between refreshes. Database/settings
  health probes have a five-second deadline; convergence uses the ordinary
  five-minute job bound. A provider timeout retains unprocessed package
  fragments for retry and never classifies healthy shared storage as
  unavailable.
- Extend composition only when a kernel-owned Phase requirement adds a real
  lifecycle service.

- Service indexing sends the selected package/commit set through the complete
  ordered hook chain in one Worker invocation. Results are keyed by package;
  foreign or missing owners are rejected, failed/invalid fragments retain their
  accepted state, and healthy fragments publish independently. Pending repair
  batches only the remaining owners. The first startup/recovery reindex fills
  the whole local index; startup does not repeat a completed bootstrap pass.
- Restore inherited workload state before schema jobs or package commands can
  start. Startup recovery must never classify a new job's normal sandbox
  deletion as an inherited runtime failure.

# Work Guidance

- Keep imports from the `.development/generated` build module out of authored
  code by accepting generated catalogs/settings in `Config`; kernel
  runtime-protocol consumers use the generator-maintained mirror under
  `kernel/runtime/protocol`. Process composition must never depend on the
  platform source tree or execute runtime/image construction scripts.

# Verification

- `main_test.go` builds the real logd companion once; disposable instance roots
  must fit the Unix socket pathname bound. `control_plane_test.go` proves the
  command socket remains usable while runtime initialization is deliberately
  blocked, observes the atomic transition to ready, then verifies live shutdown
  status and mutation rejection during cleanup. `integration_test.go` covers
  socket readiness, status, both admin modes, complete interactive help,
  compact/detailed settings lists, precedence, live HTTP/SSH listener and
  logging changes, bounded log queries/continuation, root alias redirection and
  validation, occupied-port rollback, separate node/global persistence through
  the same commands, persistence across restart, unset, shutdown/restart
  instructions, and cleanup; `runtime_test.go` covers startup failure
  propagation, healthy-sandbox selection, ordered cleanup stages, and concurrent
  controller cleanup.
- `runtime_test.go` also verifies shared-cache retention across service/job
  profiles and recreation while preserving private bounded SQLite storage.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
