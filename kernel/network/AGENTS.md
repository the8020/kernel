# Purpose

- Own the main HTTP listener, live port replacement, health endpoints, and Phase
  1B dynamic service path routing.

# Ownership

- Own `0.0.0.0:<network.main_port>`, the configured `GET /` redirect,
  `GET /health`, listener serving, replacement, a synchronized longest-prefix
  route table, and graceful close.
- Do not own the administrative Unix socket, service Worker selection, or
  request execution.

# Local Contracts

- Public API: `ErrPortUnavailable`, `Manager`, `New`, `Port`, `RegisterRoute`,
  `UnregisterRoute`, `Prepare`, and `Close`.
- Replacement binds the candidate while the old listener remains active; commit
  serves/switches then gracefully closes the old server; discard closes only the
  candidate.
- Reapplying the active port is a no-op.
- Dynamic routes survive listener replacement and cannot replace `/` or
  `/health`.
- `/` issues a `307` redirect to the configured canonical relative alias path
  and preserves the request query; `/health` continues to return plain `OK`.
- `Close` atomically clears the active listener before graceful server drain, so
  concurrent shutdown status reports `main_port=0` safely.

# Work Guidance

- Keep the main application listener on all IPv4 interfaces so container and
  host port publication reaches it; keep administrative and internal runtime
  listeners outside this package private. Use standard `net/http`.

# Verification

- `network_test.go` covers the all-interface bind, root redirect
  validation/query preservation, the independent health endpoint,
  longest-prefix dynamic routing, route
  removal/replacement survival, successful listener replacement, old-port
  release, occupied-port rollback, no-op replacement, and cleared port status
  after close.

# Child DOX Index
