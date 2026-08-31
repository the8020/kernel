# Purpose

- Own full-mode Linux network namespaces/CNI/firewall state and reduced-mode loopback control-endpoint allocation.

# Ownership

- Full mode creates deterministic namespaces through `iproute2`, applies the pinned CNI chain and nftables policy, and persists cleanup records. Rootless mode persists distinct supervisor/inspector loopback ports without claiming a namespace or firewall.
- Do not expose host ports, proxy service traffic, create containerd tasks, resolve runtime groups, or grant Deno network permissions.

# Local Contracts

- Public API: `New`, `NewLoopback`, both managers' allocate/check/release methods, `NewNFTFirewall`, `NFTFirewallConfig`, and allocation/config types.
- Allocation is transactional: failures remove firewall/CNI/netns state; release is idempotent and uses persisted records so restart cleanup remains possible.
- Network namespace and firewall names derive from validated runtime-group IDs and instance UUIDs; commands never interpolate shell text.
- Every sandbox may send authenticated callbacks to the exact configured kernel endpoint and established response traffic; kernel-to-supervisor traffic is allowed while other unsolicited ingress is dropped. All remaining sandbox forwarding is default-denied even when egress is enabled without targets.
- Restricted egress resolves explicit IPv4/CIDR/hostname targets, rejects addresses or networks inside/overlapping the sandbox subnet, honors optional ports, permits DNS only to host resolvers when names are used, then drops the remainder.

# Work Guidance

- Keep CNI, command execution, and loopback reservation behind narrow interfaces for deterministic tests; host networking is allowed only in the explicitly selected and status-disclosed rootless mode.

# Verification

- Unit tests cover CNI setting overrides, allocation/result conversion, rollback, durable records, idempotent release, callback/established allowances, unsolicited-ingress and sandbox-to-sandbox denial, empty-policy default denial, command arguments, hostname/port resolution, and nftables rules. Privileged integration tests verify real namespaces, CNI, connectivity, isolation, and cleanup.

# Child DOX Index
