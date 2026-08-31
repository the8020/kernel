# Purpose

- Expose Phase 1C filesystem service discovery, lifecycle, validation, testing, and OpenAPI.

# Ownership

- Own declarative service list/inspect/validate/start/stop/restart/scale/request/OpenAPI handlers.

# Local Contracts

- Service IDs and canonical prefixes are filesystem-derived; commands cannot supply entrypoints, route prefixes, package IDs, or source mounts.
- `service list` exposes only service identity, description, canonical path, state, enabled status, and local instance/Worker counts; `service inspect` owns generations, instance identities, effective configuration, failures, and metrics.
- Lifecycle commands return generation/capacity summaries by default and expose complete status only through `--detail`.
- `service scale` mutates replica and Worker-per-replica bounds, concurrency per
  Worker, target utilization, persistent keep-alive, and sandbox group through
  validated desired state.
- `service request` uses the ordinary canonical kernel boundary and never invokes a handler directly.

# Work Guidance

- Delegate filesystem state, rolling replacement, pool bounds, scheduling, and cleanup to the high-level web-service manager.

# Verification

- Generated validation and handler tests cover every service command.

# Child DOX Index

- This domain contract owns its leaf command folders; they contain only one declarative command and thin handler each.
