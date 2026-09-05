Parent DOX: [kernel/defaults/config/runtime DOX](../AGENTS.md).

# Purpose

- Own the tracked canonical CNI network-list template copied into the
  development runtime installation.

# Ownership

- Define the Phase 1B bridge, host-local IPAM range, default route, and loopback
  chain.
- Do not install plugin binaries, allocate namespaces, expose ports, or define
  application routing.

# Local Contracts

- `the8020.conflist` uses CNI spec 1.1.0, a dedicated `the8020` bridge,
  host-local `10.88.0.0/16`, and loopback.
- Runtime installation copies this file unchanged to the explicit configured CNI
  directory.

# Work Guidance

- Keep the configuration deterministic and free of host-specific absolute paths.

# Verification

- Host preflight parses the copied network list and integration tests allocate
  and clean an actual namespace.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
