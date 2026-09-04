# Purpose

- Adapt trusted runtime package calls to kernel-owned implementation primitives.

# Ownership

- Own the private operation name to implementation mapping used by `@the8020/kernel`.
- Do not publish CBus commands, parse package command lines, or define package policy.

# Local Contracts

- Operations call handlers or managers directly; they never recurse through the public command registry.
- Settings operations enforce their declared global or node storage boundary.
- Password hashing preserves the configured Argon2id PHC format and never returns plaintext.
