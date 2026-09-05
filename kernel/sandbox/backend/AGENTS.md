Parent DOX: [kernel/kernel/sandbox DOX](../AGENTS.md).

# Purpose

- Define the host-authoritative sandbox backend boundary and contain concrete
  full containerd and direct rootless runsc implementations.

# Ownership

- Own backend lifecycle contracts shared by the sandbox manager and
  backend-specific child packages.
- Do not own grouping, Worker scheduling, CNI allocation, command rendering, or
  Deno supervisor logic.

# Local Contracts

- A backend implementation must scope every managed object to the kernel
  instance UUID, select gVisor explicitly, expose
  create/start/observe/owned-metadata-list/stop/kill/delete operations, and
  narrowly update owner, owner-list, logical-service-list, placement-group, and
  warm-assignment labels as shared-group membership changes.
- Production backends implement the optional `ConsoleBackend` contract to exec
  one process with a direct argument vector, environment, and absolute working
  directory, using either byte-transparent streams or bounded PTY geometry.
  Attached streams expose distinct stdout/stderr, stdin half-close, and the real
  process exit status to their transport. Lifecycle-only fakes do not need to
  implement it.
- The shared OCI helpers own Deno parent permissions, stable node identity,
  control endpoints, dependency-mode environment, and bounded mount conversion.
  Mount conversion emits a deterministic parent-before-descendant order so
  overlapping targets work identically in both concrete backends. The supervisor
  receives Deno read/write permission for the exact callback socket because Deno
  requires both for Unix-socket connection; the surrounding bind mount remains
  read-only. Service supervisors alone may execute the pinned Deno binary for
  in-sandbox type checking; application Workers never receive run permission.
- Backend calls are context bounded and idempotent where lifecycle
  reconciliation requires retries.
- `ListOwned` enumerates instance-owned metadata without task or supervisor
  health probes so default crash-restart destruction is independent of stale
  runtime responsiveness; `List` remains the full observed-task inventory.

# Work Guidance

- Keep pure OCI construction testable without a live daemon; reserve real-daemon
  behavior for privileged integration tests.

# Verification

- Child-package unit tests verify OCI security/resource construction, ownership
  filtering, lifecycle, and metrics; integration tests verify the concrete
  runtimes.

# Child DOX Index

- [containerd/AGENTS.md](containerd/AGENTS.md): official containerd Go-client
  backend selecting `io.containerd.runsc.v1`.
- [rootless/AGENTS.md](rootless/AGENTS.md): direct node-local runsc backend
  selecting rootless systrap.
- [runscconsole/AGENTS.md](runscconsole/AGENTS.md): shared attached stream and
  detached console-socket runsc exec ownership.
