Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Adapt service discovery, lifecycle, validation, testing, and OpenAPI
  primitives for package runtime operations.

# Ownership

- Do not publish CBus metadata; `the8020/services` owns visible `services.*`
  command programs.
- Retain thin list/inspect/refresh/validate/request/OpenAPI adapters only. Deno
  services owns configuration and start/stop/restart/scale/defaults commands.

# Local Contracts

- Runtime list/inspect read accepted identity and supervisor observations;
  refresh performs bounded probes of the selected service's sandboxes.
- List includes the accepted package owner and source entrypoint, allowing Deno
  package administration to compose its views without service declarations in
  generic package records or a separate inspection call for every service.
- `service request` uses the ordinary canonical kernel boundary and never
  invokes a handler directly.

# Work Guidance

- Delegate only runtime observations and execution operations to webservices;
  never restore Go application policy, persistence, or command implementations.

# Verification

- Handler, package, and service-domain tests cover every operation.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.

- Leaf folders retain only thin private handlers.
