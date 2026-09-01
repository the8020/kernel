# Purpose

- Group the accepted Phase 1 command definitions and thin handlers by
  responsibility.

# Ownership

- Own system/settings, bootstrap authentication, named secrets, node topology/capacity,
  runtime, sandbox, Worker,
  filesystem package/service, development workspace/draft/activation,
  package repository, job, port, debug, and pool definitions/handlers.
- TOML files are the complete command documentation; Go handlers only extract typed arguments, delegate, and shape results/errors.

# Local Contracts

- Every leaf command package has an explicit `command.toml` handler file and constructor symbol.
- No handler reimplements parsing, persistence, rebinding, rotation, transport, or lifecycle composition.
- Resource collection commands return stable scalar summaries only; complete per-resource records belong to their `inspect` command, while aggregate runtime status exposes counts instead of embedded collections.
- Do not add commands outside the accepted phase requirements.

# Work Guidance

- Prefer direct service delegation over handler abstractions.

# Verification

- Generator tests validate the exact accepted command catalog; aggregate tests invoke handlers successfully and runtime-dependent handlers against degraded runtime state; domain and application integration tests exercise behavior.

# Child DOX Index

- `system/AGENTS.md`: system status, restart, and shutdown groups.
- `settings/AGENTS.md`: settings query and mutation groups.
- `auth/AGENTS.md`: bootstrap-administrator and authentication-session administration.
- `runtime/AGENTS.md`: runtime diagnostics, image state, eval/run, and aggregate status.
- `sandbox/AGENTS.md`: sandbox inventory, inspection, resources, and lifecycle.
- `worker/AGENTS.md`: generic Worker inventory and termination.
- `service/AGENTS.md`: filesystem service lifecycle, scaling, validation, request testing, and OpenAPI export.
- `job/AGENTS.md`: job execution records and cancellation.
- `port/AGENTS.md`: host-port leases.
- `debug/AGENTS.md`: inspector targets and debug leases.
- `pool/AGENTS.md`: warm-pool accounting and resize.
- `package/AGENTS.md`: filesystem package discovery and package-repository
  administration.
- `secret/AGENTS.md`: global named-secret list/get/set administration.
- `development/AGENTS.md`: development image, sandbox, draft, and activation
  commands.
- `node/AGENTS.md`: shared application-server topology and advertised capacity.
- `internal/commandutil/AGENTS.md`: shared internal Phase 1B handler conversions and errors.
