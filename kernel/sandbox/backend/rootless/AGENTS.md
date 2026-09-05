Parent DOX: [kernel/kernel/sandbox/backend DOX](../AGENTS.md).

# Purpose

- Run 80|20 sandboxes directly with rootless gVisor systrap when the full
  containerd/CNI/cgroup host is unavailable.

# Ownership

- Own direct `runsc` OCI bundles, per-sandbox root overlays, instance-scoped
  runtime metadata, lifecycle commands, observation, labels, logs, and rootless
  metrics.
- Do not claim CNI network isolation or hard cgroup enforcement; those
  guarantees belong to the full containerd backend.

# Local Contracts

- Public API: `New`, `Backend.Close`, the shared sandbox backend lifecycle
  methods, and generic `OpenConsole` PTY or direct-stream exec.
- Every sandbox uses the pinned node-local `runsc`, `--rootless=true`, systrap,
  host networking with loopback-only supervisor/inspector listeners, an explicit
  bounded mount set, open-only access to explicitly mounted host Unix sockets,
  empty process capabilities, and no new privileges.
- Only safe IDs and metadata carrying the current kernel instance UUID are
  observed or modified. Instance-owned metadata can be listed without invoking
  `runsc state`, allowing stale startup sandboxes to be force-deleted directly.
- Mutable metadata is limited to shared ownership, logical service membership,
  placement group, and warm-assignment labels; runtime identity remains
  immutable.
- Stop is TERM-then-KILL, delete is forced and idempotent, failed creation
  removes confirmed runtime state while retaining external logs, and the kernel
  acts as a child subreaper so cleanup does not depend on the outer container's
  PID 1. Reaping discovers only the kernel's task-owned children and never scans
  the host-wide `/proc` directory.
- Rootless memory and PID observations come from runsc. CPU usage is summed from
  `schedstat` for the same bounded set of kernel-owned sandbox/gofer tasks.
  These observations are diagnostic only and never influence placement.
- Console exec validates instance ownership and live runsc state, then delegates
  either detached console-socket PTY transfer or attached byte-transparent
  streaming to the shared runsc console package; closure affects only that exec
  process.

# Work Guidance

- Keep command execution injectable and OCI construction testable without
  launching gVisor.

# Verification

- Unit tests cover OCI restrictions, path mapping, ownership, lifecycle
  commands, bounded subreaper-child discovery, state conversion, and metrics.
- The opt-in Linux E2E test starts the real supervisor through rootless gVisor,
  verifies its bind-mounted kernel Unix socket, and dispatches a discovered
  command declared in an arbitrarily named flat TOML file through the ordinary
  job and Worker managers. It checks system user, unchanged job profile,
  static/dynamic imports from another package, normal temp/cache access, no
  invocation package artifacts, and Worker cleanup. The same harness executes
  ordered hooks from separate packages in one ordinary dispatcher job, checks
  shared object/Worker identity, reruns through ordinary sandbox reuse, changes
  handler source/revision, and checks failure identity and cleanup. Run with
  `THE8020_RUNSC_E2E=1` and absolute `THE8020_RUNSC_PATH` and
  `THE8020_RUNTIME_ROOTFS` paths to the pinned runtime and current prepared
  image. Installation also runs real rootless gVisor smoke and browser-console
  tests.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
