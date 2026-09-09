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
- Runtime composition registers the local log reader through `SetLogReader`.
  `QueryLogs` selects one exact node and uses the same authenticated recipient
  listener. The target delegates to logd and never reads files in the kernel.
- Runtime composition registers physical terminal cleanup through
  `SetTerminalCloser`. Exact-node close uses that same private recipient
  transport and does not require a surviving display Worker.

# Local Contracts

- Every node has one stable canonical `nod-` ID, public URL, recipient address
  and port, and enabled state. Local construction and catalog writes use the
  shared identity validator.
- End-user `the8020-authorization` and `the8020_auth` cookies survive every
  forwarding hop unchanged; peer `Authorization` is verified and stripped before
  service routing. The deployment signing key is separate from peer credentials.
- Recipient listeners accept only the shared authenticated kernel transport and
  proxy both HTTP and WebSocket traffic without interpreting service protocols.
- Node forwarding preserves client encoding preferences and encoded response
  bytes/headers. Its owned HTTP transport disables automatic gzip negotiation
  and decompression, so forwarding adds no compressor or decoder between the
  Deno HTTP server and the client.
- Exact Worker forwarding validates the target node and bounded envelope,
  including its canonical effective user, parent context, and optional
  persistent-execution target, dispatches directly to the registered local
  invoker, and returns structured opaque results without scanning nodes,
  sandboxes, or Workers.
- `WorkerInvocationRequest.Validate` owns canonical `nod-`/`sbx-`/`wrk-`,
  optional `ctx-`/`pex-`, function-name and principal validation. Both sending
  and receiving node transports, callbacks, and local Worker dispatch use it
  before execution. Transports separately bound encoded payloads.
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
- Native authentication transport uses the same trust boundary: proxying
  regenerates `the8020-internal-local-authentication` from private context and
  authenticated recipients restore it to context before removing the header.
  Public requests cannot promote leaked local tokens to remote credentials.
- Spillover excludes nodes already present in the forwarding path, queries
  remaining peers concurrently, ignores unreachable/non-accepting peers, and
  prefers the greatest advertised Worker then sandbox headroom.
- Listener-address changes take effect after kernel restart.
- Log reads retain the file owner's filters, starting positions, continuation
  cursors and expired/unavailable states. Requests are at most 16 KiB; responses
  use the shared log control-frame bound and strict record limits. Validate the
  responding node and preserve caller cancellation. There is no fan-out or
  alternate-node retry for missing history. Private node control never follows
  HTTP redirects with its credentials.
- The log-control route bounds body reads to two seconds, its delegated query to
  2.5 seconds and response writes to five seconds. These deadlines do not apply
  to forwarded application streams. A stalled request cannot retain a reader or
  response indefinitely.
- Terminal close validates canonical node/terminal IDs and the exact responding
  owner. Requests and acknowledgements are at most one KiB; body reads have a
  two-second deadline and the operation/response a ten-second deadline. It
  neither follows redirects nor retries another node. Application ownership,
  authorization, and metadata remain in Deno.

# Work Guidance

- Keep topology generic; route-token state, manifests, application protocols,
  and sandbox-local Worker selection remain in their owning subsystems.

# Verification

- Package tests cover deterministic database persistence, validation,
  authentication, capacity-aware service forwarding, exact local/cross-node
  Worker invocation and bounds, status collection, and allocation partitioning.
- Forwarding tests cover absent/explicit encoding preferences and unchanged
  compressed response bytes, lengths, and negotiation headers.
- Terminal tests cover authenticated exact-node close without a Worker,
  cancellation, malformed or oversized control, unavailable nodes, and bounded
  acknowledgements matching the requested identity.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
