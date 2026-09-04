# Purpose

- Adapt service discovery, lifecycle, validation, testing, and OpenAPI
  primitives for package runtime operations.

# Ownership

- Do not publish CBus metadata; `the8020/services` owns visible `services.*`
  command programs.
- Retain thin list/inspect/refresh/validate/start/stop/restart/scale/request/
  OpenAPI handlers behind the private dispatcher.

# Local Contracts

- Service IDs and canonical prefixes are filesystem-derived; commands cannot supply entrypoints, route prefixes, package IDs, or source mounts.
- `service list` exposes only service identity, description, canonical path,
  state, enabled status, version count, and unique local sandbox/Worker counts;
  `service inspect` owns versioned sandbox identities, effective configuration,
  failures, and metrics. Both use cached supervisor observations, not routing
  reservations. `service refresh` performs bounded live refresh only for the
  selected service's unique sandboxes before returning the same detail shape.
- Lifecycle commands return version/capacity summaries by default and expose complete status only through `--detail`.
- `service scale` mutates the canonical service type, minimum/maximum Workers,
  per-Worker concurrency and target utilization, Worker/session keepalives,
  sandbox group, minimum sandboxes, and Workers-per-sandbox through validated
  desired state. Zero minimum permits scale-to-zero; zero maximum is unlimited
  only at the service-policy layer.
- `service request` uses the ordinary canonical kernel boundary and never invokes a handler directly.

# Work Guidance

- Delegate desired-state changes, rolling replacement, pool bounds, scheduling,
  and cleanup to the high-level web-service manager.

# Verification

- Handler, package, and service-domain tests cover every operation.

# Child DOX Index

- Leaf folders retain only thin private handlers.
