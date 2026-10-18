Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own the main HTTP/HTTPS listeners, live port replacement, health endpoints,
  and Phase 1B dynamic service path routing.

# Ownership

- Own `0.0.0.0:<network.main_port>`, the configured `GET /` redirect,
  `GET /health`, listener serving, replacement, a synchronized longest-prefix
  route table, and graceful close.
- Own compiled HTTPS snapshots, memory-only SNI/SAN lookup, cold-handshake
  bounds, HTTP hostname admission, and the trusted-proxy evidence adapter.
- Do not own the administrative Unix socket, service Worker selection, or
  request execution.

# Local Contracts

- Public API: `ErrPortUnavailable`, `Manager`, `New`, `Port`, `RegisterRoute`,
  `UnregisterRoute`, `Prepare`, and `Close`.
- Replacement binds the candidate while the old listener remains active; commit
  serves/switches then gracefully closes the old server; discard closes only the
  candidate.
- Reapplying the active port is a no-op.
- `PrepareAdditionalHTTP(port)` uses the same router and prepare/commit/discard
  lifecycle for one additional HTTP port; zero disables it. This preserves
  existing public Docker HTTP mappings while retaining the internal main port.
  Both HTTP listeners close with the manager; neither adds a forwarding hop.
- Dynamic routes survive listener replacement and cannot replace `/` or
  `/health`.
- `/` issues a `307` redirect to the configured canonical relative alias path
  and preserves the request query; `/health` continues to return plain `OK`.
- `Close` atomically clears the active listener before graceful server drain, so
  concurrent shutdown status reports `main_port=0` safely.
- `PrepareTLS(port, config)` and `TLSPort()` manage a second native HTTPS
  listener using the same routes and availability gate. Port zero disables it.
  Every preparation must commit or discard; TLS publication is serialized
  without locking ordinary routing. Same-port publication atomically replaces an
  immutable TLS configuration for new full handshakes without draining existing
  connections. Certificate loading, selection, and policy belong to the caller.
  Use TLS 1.2+ and HTTP/2, and request optional client certificates so
  password/token clients remain supported. A new listener bounds initial
  TLS/header setup to 30 seconds.
- `HTTPS.Prepare` validates package-published public records and privately
  resolves only changed immutable keys. Commit swaps one memory snapshot;
  exact names and single-label wildcards use direct maps. Native code never
  reads certificate/account tables or performs ACME itself.
- Bound a snapshot to 10,000 certificates, 32 MiB of PEM, and 100 SANs per
  certificate. Each key is at most 64 KiB and each chain 256 KiB/16 entries.
  Keep private material out of status and errors.
- Cold handshakes share one operation per name, with at most 32 local operations
  and 128 waiting connections. Handshakes wait at most 20 seconds; ordinary
  package issuance may continue under its bounded node context after disconnect.
  System-wide admission budgets remain in the HTTPS package.
- Enforce current hostname policy during full/resumed handshakes and each HTTP
  request. Plain HTTP loopback requests for localhost/loopback names retain
  recovery access. Configured default IP handles no-SNI connections, defaulting
  to `127.0.0.1`. Canonical names unmap IPv4-mapped IPv6 addresses.
- Package-published automatic names may request issuance when ACME is disabled;
  the package retains local-CA and issuer policy. Bound that set to 16 names.
- Disabling the TLS listener retains configured HTTP hostname admission and
  trusted-proxy policy. Retiring the HTTPS package clears both policies.
- RFC 9440 `Client-Cert` is a single bounded DER byte sequence, accepted only
  from explicitly configured immediate-peer IPs or CIDRs. Strip both certificate
  headers before dispatch. Trusted evidence enters private auth context and
  responses vary by `Client-Cert`; the terminator must regenerate headers over
  a protected backend path. Forwarded chains confer no additional authority.
- The two well-known challenge paths perform only an exact package lookup,
  before hostname admission. Bound tokens, response size, 16 local lookups, and
  five-second lookup deadlines; responses are not cached.
- Challenge query `local=1` passes a local-only lookup flag to the package,
  preventing repeated forwarding between independently configured systems.
- `HTTPSStatus` reports desired/applied revisions, readiness, enabled state,
  loaded certificate count, and bounded publication failure from memory.

# Work Guidance

- Keep the main application listener on all IPv4 interfaces so container and
  host port publication reaches it; keep administrative and internal runtime
  listeners outside this package private. Use standard `net/http`.

# Verification

- `network_test.go` covers the all-interface bind, root redirect
  validation/query preservation, the independent health endpoint, longest-prefix
  dynamic routing, route removal/replacement survival, successful listener
  replacement, old-port release, occupied-port rollback, no-op replacement, and
  cleared port status after close.
- `tls_test.go` exercises real TLS, resumption and client key possession,
  HTTP/2, optional client certificates, certificate replacement on retained
  connections, rejected provider/port changes, prepare/discard, and graceful
  close.
- `certificates_test.go` covers parsed-key reuse, live/resumed admission,
  concurrent cold-handshake coalescing, trusted/forged forwarding headers, and
  exact challenge routing through the normal transport implementation.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
