# Purpose

- Own host/runtime compatibility, the pinned version manifest and
  development-image identity, full-image and portable-rootfs records,
  sandbox-mode selection, and runtime health diagnostics used by the kernel and
  administration commands.

# Ownership

- Load and validate node-local `node/kernel/runtime/definitions/versions.toml`, resolve
  architecture-specific artifact checksums, inspect full
  Linux/containerd/gVisor/CNI/cgroup and node-image readiness plus direct
  node-local runsc/rootfs/capability/seccomp readiness, select `auto`, `full`, or
  `rootless`, expose structured reports, own the generated kernel-side runtime
  protocol, and own authenticated supervisor callback ingress over the
  node-private Unix socket.
- Do not create sandboxes, schedule Workers, install host packages, build images, or execute application code.

# Local Contracts

- Public API: `LoadVersions`, `Versions.Checksums`, `NewDoctor`, `NewRootlessDoctor`, `SelectMode`, `NewIsolationReport`, and the version/report model types.
- The installed node-local manifest originates from the tracked default and is
  authoritative for that node; floating versions, malformed artifact
  checksums, unsafe development-image records, unsupported
  architectures, and protocol/image schema mismatches fail explicitly.
- Diagnostics read only already materialized records and artifacts beneath
  `node/kernel/runtime/images/`. This package never executes Deno, downloads a
  tool, stages source, or invokes an image-build script.
- Diagnostics are side-effect free and bounded by caller context; full
  privileges require effective `SYS_ADMIN` and `NET_ADMIN`, while rootless
  readiness requires `SYS_CHROOT`, compatible seccomp, the pinned node-local
  runsc/rootfs, and a matching smoke record.

# Work Guidance

- Keep operating-system probes injectable and deterministic in tests while production defaults inspect the real host.

# Verification

- Unit tests cover strict manifest parsing, pin validation, architecture/gVisor-release resolution, healthy and degraded host reports, image identity, smoke status, and the image-smoke/installer source contracts.

# Child DOX Index

- `callback/AGENTS.md`: authenticated job/service runtime API over the
  bind-mounted Unix socket.
- `protocol/AGENTS.md`: generated Go runtime-protocol envelope and message types.
