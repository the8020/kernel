# Purpose

- Implement sandbox container and task lifecycle through the official containerd Go client and the gVisor shim.

# Ownership

- Own containerd connection/version/image checks, instance namespace and labels, immutable root snapshots, OCI spec construction, task I/O capture, signals, observation, and cleanup.
- Do not allocate CNI networks, expose host ports, persist desired state, schedule Workers, or communicate with the Deno supervisor.

# Local Contracts

- Public API: `Connect`, `NamespaceForInstance`, and `Backend`
  lifecycle/image/observation plus generic `OpenConsole` methods.
- Every created container explicitly uses `io.containerd.runsc.v1`, an immutable image digest, read-only rootfs, no new privileges, empty capabilities, bounded mounts, cgroup-v2 limits, one workload type, and the configured supervisor heartbeat/Worker-stop intervals.
- Only containers in the derived namespace carrying matching managed and instance labels are returned or modified. Ownership-only listing reads labels without querying task state for the default restart-destruction path.
- Post-create label mutation is limited to owner, shared-owner list, logical
  service list, group key, and warm-assignment timestamp metadata; runtime
  identity labels remain immutable.
- Create failures clean partial task/container/snapshot state; stop is graceful then forced, kill is immediate, and delete removes the task before the snapshot-backed container.
- Console exec uses containerd task exec with either a terminal or transparent
  pipe I/O according to the caller; resize and close address only the exec
  process and never the owning task.

# Work Guidance

- Keep containerd sockets and host paths out of OCI mounts and supervisor environment; never add a native Deno fallback.

# Verification

- Unit tests cover namespace derivation, OCI security/resources/mounts/environment/network namespace, mutable shared-owner metadata, reserved labels, dependency mode, and ownership filtering. Privileged tests use real containerd and gVisor.

# Child DOX Index
