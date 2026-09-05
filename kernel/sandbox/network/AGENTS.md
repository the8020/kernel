Parent DOX: [kernel/kernel/sandbox DOX](../AGENTS.md).

# Purpose

- Own full-mode Linux network namespaces/CNI/firewall state and reduced-mode
  loopback control-endpoint allocation.

# Ownership

- Full mode creates deterministic namespaces through `iproute2`, applies the
  pinned CNI chain and nftables policy, and persists cleanup records. Rootless
  mode persists distinct supervisor/inspector loopback ports without claiming a
  namespace or firewall.
- Do not expose host ports, proxy service traffic, create containerd tasks,
  resolve runtime groups, or grant Deno network permissions.

# Local Contracts

- Public API: `New`, `NewLoopback`, both managers' allocate/check/release
  methods, `NewNFTFirewall`, `NFTFirewallConfig`, and allocation/config types.
- Allocation is transactional: failures remove firewall/CNI/netns state; release
  is idempotent and uses persisted records so restart cleanup remains possible.
- Network namespace and firewall names derive from validated runtime-group IDs
  and instance UUIDs; commands never interpolate shell text.
- Runtime callbacks use a bind-mounted Unix socket and need no network firewall
  exception. Kernel-to-supervisor traffic and established responses are allowed
  while unsolicited ingress and sandbox-to-sandbox traffic are dropped.
- Egress enabled with no targets means unrestricted outbound access. A nonempty
  target list enables restricted egress: resolve explicit IPv4/CIDR/hostname
  targets, reject overlap with the sandbox subnet, honor optional ports, permit
  DNS to host resolvers for names, then drop the remainder.

# Work Guidance

- Keep CNI, command execution, and loopback reservation behind narrow interfaces
  for deterministic tests; host networking is allowed only in the explicitly
  selected and status-disclosed rootless mode.

# Verification

- Unit tests cover CNI setting overrides, allocation/result conversion,
  rollback, durable records, idempotent release, unrestricted empty-target
  egress, restricted targets, established responses, unsolicited-ingress and
  sandbox-to-sandbox denial, hostname/port resolution, and nftables rules.
  Privileged integration tests verify real namespaces, CNI, connectivity,
  isolation, and cleanup.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
