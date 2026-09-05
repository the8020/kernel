Parent DOX: [kernel DOX](../AGENTS.md).

# Purpose

- `kernel/` is the small Go layer running on every application-server node.
- 80|20 separates this OS-authoritative kernel from programs that will all run
  under one Deno execution, isolation, permission, lifecycle, debugging, and
  dispatch model.
- Phase 1A proves local installation, instance locking, configuration,
  administration, networking, logging, and lifecycle. Phase 1B adds
  containerd-managed or rootless gVisor sandboxes and one Deno supervisor/Worker
  execution path. Phase 1C adds filesystem packages and persistent web services.
  Phase 1D adds authentication, access-controlled stateless/persistent services,
  and the UUI application protocol on that same runtime. Phase 1E adds private
  development sandboxes, persistent private overlays, independent package Git
  repositories, typed activation and synchronization, and kernel-authenticated
  SSH terminal access.

# Ownership

- The kernel owns process and node lifecycle, local and distributed request
  routing primitives, listeners, OS integration, resource control, runtime
  configuration, logging, runtime-process communication, low-level
  filesystem/storage access, and the administrative command bus when those
  capabilities require node authority.
- Phase 1B extends the existing foundation with host-authoritative sandbox,
  execution, port, routing, resource, and debugging behavior while all program
  code remains inside Deno Workers; full containerd mode is preferred and direct
  rootless gVisor is the automatic reduced fallback.
- The kernel additionally owns development-image/sandbox lifecycle, durable
  per-user sandbox storage, sandbox-scoped activation ingress, database-backed
  package persistence and global named-secret storage, Git source/version and
  working-tree inspection, authenticated pull/push/checkout, and package
  synchronization, plus the generic authenticated local console broker and
  backend PTY exec boundary. It owns the SSH listener and protocol adapter while
  authentication, development, and sandbox packages retain their domain
  behavior. Deno programs own application routes, handlers, UUI screen/program
  behavior, browser assets, and authored TypeScript table definitions; future
  phases add schema versions, broader identities/roles, workflow, connections,
  certificates, and other application behavior.
- Application query behavior, broader remote administration, TLS termination,
  checkpoint/restore, and the final virtual filesystem remain outside the
  current kernel scope; the kernel owns physical database synchronization while
  packages remain the authored schema source. Initial application-server
  topology, capacity advertisement, allocation-index partitioning, and service
  forwarding are kernel-owned.

# Local Contracts

- Every Go package has one responsibility and the smallest public API needed
  now.
- Kernel hot and periodic paths must be proportional to current work, never to
  total retained history or filesystem size. Use direct durable state, explicit
  events, bounded diagnostics, and narrow locks; reject polling scans,
  serialization-based persistence, and external I/O under unrelated global
  locks.
- Keep handlers thin and domain behavior in its owning package.
- Do not add speculative interfaces, placeholders, plugin loading,
  reflection-based discovery, frameworks, or dumping-ground packages.
- Generated Go is reproducible build glue under `.development/generated/`; the
  runtime-protocol generator also maintains its tracked TypeScript model under
  `defaults/config/runtime/protocol/generated.ts` and kernel-consumed Go mirror
  under `kernel/runtime/protocol/`. Never edit generated files manually.
  Authored kernel Go belongs under `kernel/`; native executables owned by a
  runtime image belong with that image's source.
- Kernel command TOML is authoritative for built-in IDs, paths, help, arguments,
  results, mutation/restart metadata, handlers, and examples. Active packages
  own flat `cbus/commands/*.toml` with an explicit full public `command` name
  and same-package TypeScript programs referenced by `program`; the in-memory
  CBus catalog is rebuilt atomically from active package commits.
- Native package identity and program/hook/event/command discovery remain in the
  kernel. Deno services owns service declarations, defaults, overrides,
  versions, storage, and administration. Go validates resolved runtime specs and
  atomically publishes one fragment per owning package in memory; request paths
  never read service TOML or application tables. `kernel.reindex` is the common
  boot, activation, edit, and cross-node publication path.
- Setting TOML is authoritative for keys, types, node/global storage, defaults,
  environment inputs, validation, and runtime/restart metadata.
- One runtime group is one gVisor sandbox with exactly one workload type—service
  or job—and one infrastructure Deno supervisor; application code runs only in
  Workers. There is no generic user-session workload.
- Deno services resolves the runtime specification: lifecycle type is
  `stateless` or `session`; scaling owns minimum/maximum Workers, per-Worker
  concurrency and target utilization, and Worker keepalive; placement owns
  sandbox group, minimum warm sandboxes, and Workers per sandbox. The kernel
  owns desired Worker allocation, Worker-count-based sandbox placement and
  admission, and scale-down; Deno owns only exact local Worker lifecycle and
  utilization observation. Internal persistent execution binding implements
  session services independently of HTTP or WebSocket transport and never
  interprets UUI messages. Routing selects only the loaded service version;
  occupied prior-version Workers drain without failing a version switch.
- UUI sessions are ordinary persistent service executions. UUI establishment,
  replay, heartbeats, reconnect, and program recovery live in the UUI service
  handler and do not introduce a UUI-specific sandbox or supervisor category.
- All UUI implementation and configuration, including protocol, timing, limits,
  Home, Program terminated, session metadata, and administration, belongs to the
  independent `the8020/uui` package. The kernel neither defines UUI settings nor
  injects application configuration into requests.
- Ordinary service and job sandboxes receive the activated package source
  read-only at `/workspace/packages`. Shared application data uses the typed
  kernel database bridge rather than a generic writable state mount.
- The generic event dispatcher emits local minute-boundary notifications and
  asynchronously invokes programs referenced by cached package TOML handlers. It
  owns no calendar, queue table, durable event delivery, or job history.
  `the8020/jobs` owns Deno scheduling, per-node cursors, Any claims, independent
  All executions, and result persistence. Ordinary low-level jobs remain
  non-durable. Existing activation hooks retain synchronous semantics.
- Package-index commands validate HTTPS Git sources, list bounded refs and
  commits, atomically synchronize clean mapped repositories to latest, tag, or
  commit selections, create unlinked local repositories, and reload only
  services owned by changed packages. Independent repository commands inspect,
  fast-forward pull, push, and check out a branch or commit while preserving
  dirty worktrees. The database-backed global secret store owns values; package
  records contain at most a selected secret name. Git credentials never belong
  in index documents, repository URLs, durable Git configuration, or command
  arguments.
- Supervised Deno code may invoke a registered JSON-in/JSON-out function on one
  exact node/sandbox/Worker through the generic kernel SDK. Go validates
  infrastructure identity, bounds, timeout, and forwarding while treating the
  function name and payload as opaque. Persistent handlers may report exact
  execution completion so generic binding are released in the owning supervisor.
  Signed route descriptors contain exact node/sandbox/Worker/execution IDs and
  can never recreate lost bindings. There is no routing database or duplicate
  kernel lease bookkeeping.
- The restart-required global `network.root_alias` setting selects the relative
  80|20 application path receiving temporary redirects from `/`; it defaults to
  `the8020/uui/shell/`, while `/health` remains an unaliased kernel endpoint.
- The main HTTP listener binds all IPv4 interfaces so container and host port
  publication can reach application traffic. Administrative command transport,
  runtime callbacks, and other explicitly internal listeners remain private.
- Public services completely ignore platform tokens and execute as their
  configured user. They preserve raw cookies/headers for explicit package
  login/logout. Authenticated services verify the platform JWT in Go before
  request-triggered capacity or dispatch, then run users-package account/session
  policy inside the existing target Worker before HTTP handling or WebSocket
  acceptance.
- The kernel owns cryptographic integrity and private deployment key storage;
  the users Deno package owns passwords, sessions, revocation, login/logout, and
  application cookies. Context getters are synchronous and never authenticate.
  Do not introduce another auth service, Worker, sandbox, package copy, or
  parallel execution mechanism. Extend shared service/job capabilities at their
  owning layer when a real capability is missing.
- Current 80|20 package code is trusted. Jobs and services receive the same
  typed Deno-to-kernel API, immutable execution-context API, and unrestricted
  outbound network/import access. Every Worker has a validated user and
  service/job/program origin; authenticated requests replace the service's
  configured anonymous user with their exact identity for that invocation.
  Kernel principals are structural identities independent of users-package
  tables for every username, including `system`. Runtime validation checks
  canonical shape and assignment only; it never checks account existence or
  enabled state. There is no granular capability system yet. Runtime operations
  and `admin.execute` require an active supervised Worker execution, not a
  service HTTP request or authenticated user. Signing and verification use this
  same bridge for both workloads.
- The kernel owns one dynamically opened `database/sql` pool. Backend, location,
  and PostgreSQL credentials are restart-required node-local policy; maximum
  open and retained-idle connections are runtime-mutable node policy, defaulting
  to 32 and 8. SQLite uses WAL in its private local database directory;
  PostgreSQL remains the shared multi-node backend.
- The built-in `_8020_*` catalog, readiness state, physical introspection,
  additive synchronization, deployment locking/recovery, bounded query bridge,
  and execution-scoped transactions are kernel-owned. A fresh database loads
  every installed package table before services start; normal boots do not
  rescan definitions. Package changes synchronize only affected definitions
  before source activation. Ordinary removals retire data until an explicit
  confirmed trim.
- The Go kernel owns backend selection, containerd access when available, direct
  runsc lifecycle otherwise, host ports, network state, cgroups, mounts, and
  runtime reconciliation. Neither Deno nor program Workers receive the
  containerd socket or direct host-port control.
- The loopback main listener exposes one authenticated, same-origin generic
  console WebSocket. It may exec a bounded direct argument vector in any ready
  development or runtime sandbox, streams PTY bytes and resize controls, and
  exposes no SSH credential, sandbox token, host port, or forwarding feature.
  All authenticated users currently have console access; granular policy is
  deferred to the full permission system.
- The administrative control plane starts independently of runtime recovery.
  Default restart force-deletes inherited instance-owned sandboxes without
  supervisor probes, publishes runtime readiness asynchronously, and creates
  replacement sandboxes only from explicit workload demand; cross-restart
  restoration is opt-in.
- Kernel initialization creates and validates only the instance layout and
  node-local state. Startup loads and validates
  `node/kernel/runtime/definitions/versions.toml` plus already materialized
  records beneath `node/kernel/runtime/images/`; it never runs Deno, shell image
  builders, package builds, or source staging.
- Runtime-record recovery is isolated by service. Only current service records
  are restored, malformed or obsolete records are quarantined, and restoration
  failures remain visible without aborting unrelated service routing or runtime
  composition. Ordinary jobs are non-durable and never replay. Terminal
  service-pool records are removed after owner release so missing inherited
  sandboxes do not remain retry gates.
- During graceful shutdown the administrative socket remains available for
  `kernel.status` and idempotent `kernel.shutdown` and `kernel.restart` calls
  until late process teardown, while all other command intake is rejected. The
  first lifecycle request selects shutdown or restart. Status exposes
  synchronized stage completion, restart intent, and the currently active
  cleanup message.
- CPU and memory usage remain observable but never limit sandbox creation,
  placement, or Worker admission and receive no cgroup ceilings. Full mode
  retains PID control; rootless mode reports that PID cgroup enforcement and
  CNI/firewall isolation are unavailable. Shared grouping is explicit and shares
  one failure, security, permission, and resource boundary.
- System-shipped and user-developed programs use the same supervisor and Worker
  path. Package command dispatch submits an ordinary `system` job against the
  shared package mount; it owns no source copies, mount overlays, or special
  sandbox policy. Cross-package imports use the same access as other jobs.
- A synchronous hook chain is one ordinary system job running the Deno hook
  dispatcher. Indexed handlers share state in one Worker, ordered by numeric
  declaration order and stable identity. Normal job mounts, permissions, and
  release-aware reuse apply; no handler-specific execution backend is allowed.
- Development sandboxes use the same selected rootful or rootless runsc mode as
  workload isolation but a distinct editable image and lifecycle. Their writable
  package view never grants direct publication into shared package repositories;
  checkpointed private deltas, native durable system/home storage, and Git
  activation remain separately owned. Sandbox lifecycle scans package content
  only at explicit checkpoint boundaries and never polls it.

# Work Guidance

- Prefer standard library behavior, deletion, explicit composition, compile-time
  registration, and shared parsing/validation.
- External dependencies are limited to TOML decoding, official Go cryptography
  and terminal packages, golang-jwt/v5, Linux syscalls, the official containerd
  Go client and its OCI dependencies, the pgx PostgreSQL driver, the CGo-free
  modernc SQLite driver, and narrowly scoped runtime/network libraries required
  by current behavior.
- Update the closest package DOX contract whenever ownership, APIs, invariants,
  lifecycle, extension, or verification changes.

# Verification

- `./install.sh` generates, formats, tests, and compiles the complete local
  kernel.
- `go test ./kernel/...` runs authored package and integration tests when the
  repository-local Go environment is configured.
- Phase 1 completion also requires behavioral verification of both binaries and
  `run.sh`, including a real PTY `Ctrl-C` shutdown-progress smoke.

# Child DOX Index

- [admin/AGENTS.md](admin/AGENTS.md): one-shot and interactive administrative
  client.
- [app/AGENTS.md](app/AGENTS.md): process composition and startup/shutdown
  ordering.
- [auth/AGENTS.md](auth/AGENTS.md): private deployment signing key, JWT profile,
  and credential transport.
- [cbus/AGENTS.md](cbus/AGENTS.md): typed administrative command bus and
  generation hierarchy.
- [console/AGENTS.md](console/AGENTS.md): transport-neutral sandbox PTY leases
  and the authenticated local WebSocket relay.
- [database/AGENTS.md](database/AGENTS.md): connection pool, catalog, schema
  synchronization, runtime SQL, values, and transaction scopes.
- [debugging/AGENTS.md](debugging/AGENTS.md): inspector-target mapping and
  temporary loopback-only debug leases.
- [deployment/AGENTS.md](deployment/AGENTS.md): Define the small
  schema-deployment handshake shared by package synchronization and development
  activation.
- [development/AGENTS.md](development/AGENTS.md): the per-user development
  sandbox, private package overlay checkpoints, package Git activation, and
  reset behavior.
- [events/AGENTS.md](events/AGENTS.md): asynchronous local package events,
  cached listeners, bounded dispatch, and minute-aligned notification.
- [execution/AGENTS.md](execution/AGENTS.md): generic runtime groups, warm
  capacity, supervisor communication, Workers, services, and jobs.
- [instance/AGENTS.md](instance/AGENTS.md): mapped instance paths,
  initialization, identity, lock, and cleanup.
- [lifecycle/AGENTS.md](lifecycle/AGENTS.md): shutdown state and notification.
- [logging/AGENTS.md](logging/AGENTS.md): slog writer, rotation, retention, and
  policy replacement.
- [network/AGENTS.md](network/AGENTS.md): proof HTTP listener and port
  replacement.
- [nodes/AGENTS.md](nodes/AGENTS.md): shared node topology, capacity
  advertisement, allocation-index partitioning, and authenticated HTTP/WebSocket
  forwarding.
- [packages/AGENTS.md](packages/AGENTS.md): native package/program/handler
  discovery, desired and active package state, Git activation, identity, and
  generic index revisions.
- [ports/AGENTS.md](ports/AGENTS.md): host-owned port leases, persistence,
  expiration, and streaming proxies.
- [runtime/AGENTS.md](runtime/AGENTS.md): pinned manifest loading, full/rootless
  readiness diagnostics, and mode selection.
- [sandbox/AGENTS.md](sandbox/AGENTS.md): sandbox models, full containerd and
  direct rootless gVisor backends, resources, mounts, state, and lifecycle.
- [secrets/AGENTS.md](secrets/AGENTS.md): private global named-secret
  persistence and value access.
- [services/AGENTS.md](services/AGENTS.md): typed handler dependencies.
- [settings/AGENTS.md](settings/AGENTS.md): definitions, precedence,
  persistence, queries, and runtime transactions.
- [ssh/AGENTS.md](ssh/AGENTS.md): authenticated SSH listener, fixed
  target-selector grammar, persistent host key, and sandbox PTY relay.
- [webservices/AGENTS.md](webservices/AGENTS.md): Phase 1C reconciliation,
  canonical service routing, lifecycle, administration, and node-local status.
