Parent DOX: [kernel/kernel/cbus DOX](../AGENTS.md).

# Purpose

- Own the minimal built-in command set and implementation adapters reused by
  private runtime operations.

# Ownership

- Public TOML remains only for `kernel.*` recovery/config/lifecycle/events plus
  the explicitly deferred debug, job, pool, port, runtime, sandbox, and Worker
  families. Package-owned users, authentication sessions, services, packages,
  secrets, database, nodes/global settings, and development commands have no
  public Go metadata here.
- Retained Go handlers for migrated domains are private implementation adapters
  used by `runtime/operations`; they never register themselves.

# Local Contracts

- Every public leaf has an explicit `command.toml` handler file and constructor
  symbol. A retained private leaf may contain only its thin handler.
- No handler reimplements parsing, persistence, rebinding, rotation, transport,
  or lifecycle composition.
- Resource collection commands return stable scalar summaries only; complete
  per-resource records belong to their `inspect` command, while aggregate
  runtime status exposes counts instead of embedded collections.
- Do not add commands outside the accepted phase requirements.

# Work Guidance

- Prefer direct service delegation over handler abstractions.

# Verification

- Generator tests validate the exact accepted command catalog; aggregate tests
  invoke handlers successfully and runtime-dependent handlers against degraded
  runtime state; domain and application integration tests exercise behavior.

# Child DOX Index

- [database/AGENTS.md](database/AGENTS.md): Adapt kernel-owned database
  primitives for package runtime operations.
- [debug/AGENTS.md](debug/AGENTS.md): inspector targets and debug leases.
- [development/AGENTS.md](development/AGENTS.md): private development
  implementation adapters.
- [internal/commandutil/AGENTS.md](internal/commandutil/AGENTS.md): shared
  internal Phase 1B handler conversions and errors.
- [job/AGENTS.md](job/AGENTS.md): live non-durable jobs and cancellation.
- [node/AGENTS.md](node/AGENTS.md): private topology implementation adapters.
- [package/AGENTS.md](package/AGENTS.md): built-in package recovery plus private
  package operations.
- [pool/AGENTS.md](pool/AGENTS.md): warm-pool accounting and resize.
- [port/AGENTS.md](port/AGENTS.md): host-port leases.
- [runtime/AGENTS.md](runtime/AGENTS.md): runtime diagnostics, image state,
  eval/run, and aggregate status.
- [sandbox/AGENTS.md](sandbox/AGENTS.md): sandbox inventory, inspection,
  resources, and lifecycle.
- [secret/AGENTS.md](secret/AGENTS.md): private named-secret implementation
  adapters.
- [service/AGENTS.md](service/AGENTS.md): private service implementation
  adapters.
- [settings/AGENTS.md](settings/AGENTS.md): node-local `kernel.config.*`
  commands.
- [system/AGENTS.md](system/AGENTS.md): kernel status, restart, shutdown,
  package declaration reindex, and local event emission.
- [worker/AGENTS.md](worker/AGENTS.md): generic Worker inventory and
  termination.
