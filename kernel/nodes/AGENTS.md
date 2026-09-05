Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own shared application-server topology, capacity advertisement,
  service-allocation-index partitioning, and authenticated node-to-node service
  forwarding.

# Ownership

- `the8020__system__nodes` is the shared, command-bus-managed node catalog.
- Running nodes refresh the catalog once per shared-state reconciliation.
  Request routing and allocation read an immutable in-memory snapshot, so a hot
  path never scans the node table. Local mutations refresh immediately.
- The shared forwarding credential is created once in the secrets table and must
  never enter a sandbox.
- The runtime supplies a read-only local capacity provider; this package exposes
  it to authenticated peers and uses peer reports only for spillover selection.
- The runtime may register a local exact-Worker invoker; this package forwards
  bounded JSON control only to the explicitly named node over the same private
  authenticated recipient transport.

# Local Contracts

- Every node has one stable ID, public URL, recipient address and port, and
  enabled state.
- End-user `the8020-authorization` and `the8020_auth` cookies survive every
  forwarding hop unchanged; peer `Authorization` is verified and stripped before
  service routing. The deployment signing key is separate from peer credentials.
- Recipient listeners accept only the shared authenticated kernel transport and
  proxy both HTTP and WebSocket traffic without interpreting service protocols.
- Exact Worker forwarding validates the target node and bounded envelope,
  including its canonical effective user and an optional persistent-execution
  target, dispatches directly to the registered local invoker, and returns
  structured opaque results without scanning nodes, sandboxes, or Workers.
- The internal capacity endpoint reports temporary-storage reservations, sandbox
  and Worker limits/counts, service sandboxes and health, and available versus
  occupied execution slots. Capacity queries are bounded and happen for
  administration or spillover, never on successful local dispatch.
- Global service-allocation indexes are partitioned round-robin over sorted
  enabled node IDs. An unconfigured local node is the single-node default; an
  explicitly disabled local node owns no indexes.
- Only authenticated recipient requests contribute forwarding history through
  trusted request context. Proxying regenerates
  `the8020-internal-forwarded-nodes`; public header values never control peer
  selection, regardless of casing.
- Spillover excludes nodes already present in the forwarding path, queries
  remaining peers concurrently, ignores unreachable/non-accepting peers, and
  prefers the greatest advertised Worker then sandbox headroom.
- Listener-address changes take effect after kernel restart.

# Work Guidance

- Keep topology generic; route-token state, manifests, application protocols,
  and sandbox-local Worker selection remain in their owning subsystems.

# Verification

- Package tests cover deterministic database persistence, validation,
  authentication, capacity-aware service forwarding, exact local/cross-node
  Worker invocation and bounds, status collection, and allocation partitioning.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
