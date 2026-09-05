Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own inspector-target discovery and temporary loopback-only debug leases for
  sandboxed Deno supervisors and Workers.

# Ownership

- Query each sandbox's allocated internal Deno inspector endpoint, map
  debugger-visible names to executions, open authenticated expiring loopback
  HTTP/WebSocket reverse-proxy leases, rewrite local target URLs, list leases,
  and close them.
- Do not implement a debugger UI, expose inspector publicly, create Workers, or
  own generic port proxying.

# Local Contracts

- Public API: `New`, `Manager.Targets`, `Open`, `List`, `Close`, and
  target/lease types.
- Debug lease creation obeys `sandbox.debug.enabled`; the configured bind must
  be a loopback IP, expiration is bounded, every lease requires a random access
  token, and the host-owned Go HTTP/WebSocket proxy exposes no sandbox token.
- Debug tokens remain in memory and returned connection data only; debug port
  records are discarded rather than unauthenticated after kernel restart.
- Inspector response bodies are size capped and malformed/remote errors fail
  explicitly.

# Work Guidance

- Preserve Deno target IDs and debugger names verbatim while deriving execution
  IDs only from the 80|20 naming convention.

# Verification

- Unit tests cover target parsing/mapping, malformed/oversized responses,
  disabled policy, token enforcement, URL rewriting, loopback/expiration
  enforcement, non-restoration, listing, and close behavior.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
