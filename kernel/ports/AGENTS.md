# Purpose

- Own host-side port leases and streaming TCP forwarding into sandbox network namespaces.

# Ownership

- Allocate automatic or explicit host listeners, validate bind policy, persist lease metadata, stream bidirectionally to sandbox IP/port, expire leases, restore safe leases, and close all listeners.
- Do not let Deno bind public host ports, allocate CNI addresses, route logical services, or map inspector targets.

# Local Contracts

- Public API: `New`, `Manager.Expose`, `ExposeHTTP`, `List`, `Close`, `CloseForSandbox`, `CloseAll`, `Restore`, `RestoreFor`, and lease/request types carrying owner, declared internal port, optional selected-backend target port, and active-state metadata.
- Public binds require explicit manager policy; automatic ports bind through Go `net.Listen`; occupied ports fail without changing lease state.
- Debug leases are intentionally discarded on restart because their authentication token and Go proxy handler are memory-only.
- Filtered restart restoration rebinds only leases approved against reconciled runtime/workload state and deletes rejected records so stale listeners cannot revive later.
- Sandbox-scoped cleanup closes only matching listeners, removes their records, and is idempotent so lifecycle failure/stop/delete paths can revoke host ingress safely.
- Proxy paths use bounded-copy streaming rather than complete buffering and dial the backend target port while retaining the declared internal port in user-facing metadata. Lease files are restrictive atomic JSON and are removed only after the listener closes.

# Work Guidance

- Keep one accept loop per lease, make close idempotent, and associate listener/dial failures with lease and sandbox IDs.

# Verification

- Unit tests cover automatic/explicit allocation, occupied rejection, TCP/HTTP streaming, individual/sandbox-scoped release, persistence/filtered restore, stale-record removal, public-bind denial, and expiration.

# Child DOX Index
