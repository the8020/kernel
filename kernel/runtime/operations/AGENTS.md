# Purpose

- Adapt trusted runtime package calls to kernel-owned implementation primitives.

# Ownership

- Own the private operation name to implementation mapping used by `@the8020/kernel`.
- Do not publish CBus commands, parse package command lines, or define package policy.

# Local Contracts

- Operations call handlers or managers directly; they never recurse through the public command registry.
- Targeted service refresh is exposed through the same typed implementation as
  its CBus command; it refreshes only the selected service's relevant
  sandboxes, never the complete runtime.
- Settings operations enforce their declared global or node storage boundary.
- Cryptographic operations delegate to the kernel deployment signer. Arbitrary
  bytes use base64 on the existing JSON bridge; JWT helpers use the explicit
  platform token profile. Private keys never cross the bridge. Password/account
  and session policy belongs exclusively to Deno packages.
- `event.emit` queues local asynchronous package listeners using the caller's
  execution user. `program.run` submits an ordinary program with inherited or
  selected user, sandbox group, and timeout, returning status/output/logs even
  on execution failure. Program selection delegates to the package catalog.
  Application schedule/history operations belong to the jobs Deno package.
