# Purpose

- Own Phase 1B gVisor sandbox identity, validation, full containerd and direct rootless lifecycle, resources, mounts, networking, and durable reconciliation state.

# Ownership

- Own sandbox/profile models, explicit states, the preferred `io.containerd.runsc.v1` backend, the direct rootless systrap backend, generic interactive PTY and byte-transparent process exec, CNI or loopback allocation, cgroup or runsc metrics, controlled mounts, state persistence, and lifecycle coordination.
- Do not own Worker scheduling, workload contracts, host-port listeners, command presentation, or Deno program logic.

# Local Contracts

- One sandbox is one managed OCI runtime, one gVisor boundary, one runtime group, one workload type, and one Deno supervisor.
- Every service sandbox has exactly one free-text placement-group value. It may
  contain replicas of compatible different services but never two replicas of
  the same logical service.
- Sandbox creation enforces node-wide sandbox-count, memory, CPU, and temporary
  storage reservation budgets before host mutation.
- Full containerd namespace/labels and rootless metadata derive from the stable kernel instance UUID; foreign or unlabeled runtimes are never managed.
- Full roots are immutable snapshots; rootless roots use a shared lower rootfs and private overlay. Mounts remain bounded and the containerd socket is never mounted.
- State transitions are explicit and synchronized; desired files, backend/task observation, supervisor health, and backend metrics remain separate evidence.
- Interactive consoles create an additional bounded process only in a ready
  sandbox; closing that console must not stop the sandbox or expose a runtime
  socket to Deno.

# Work Guidance

- Keep Linux host authority in Go, prefer `io.containerd.runsc.v1`, select direct pinned runsc only through the rootless backend, and return precise unsupported/unavailable diagnostics without native fallback.

# Verification

- Unit tests cover model/profile/state/mount/resource behavior with deterministic fakes; privileged integration tests cover real containerd, runsc, CNI, cgroups, reconciliation, and cleanup.

# Child DOX Index

- `backend/AGENTS.md`: sandbox backend contract and concrete full/rootless gVisor implementations.
- `history/AGENTS.md`: separate terminal metadata/log archives, bounded indexes,
  direct lookup, and retention cleanup.
- `model/AGENTS.md`: typed specifications, profiles, statuses, and transitions.
- `mounts/AGENTS.md`: host-source canonicalization and mount policy.
- `manager/AGENTS.md`: transactional lifecycle, readiness, inspection, metrics, reconciliation, and shutdown.
- `network/AGENTS.md`: CNI namespaces, assigned IPs, cleanup records, and nftables egress policy.
- `resources/AGENTS.md`: cgroup v2 limit generation and metrics.
- `state/AGENTS.md`: atomic local desired/observed state persistence.
