Parent DOX: [kernel/defaults/config/runtime/deno DOX](../AGENTS.md).

# Purpose

- Provide the shared TypeScript operational ID generator and validator.

# Ownership

- Own `newId(prefix)` and `isId(value, prefix)`, also exported by the kernel SDK
  for package owners. The encoding matches the Go `identity` contract: three
  lowercase letters, a dash, and ten uniformly random lowercase alphanumeric
  characters.

# Local Contracts

- Resource owners select a documented prefix and check collisions when
  registering. Short random IDs do not authorize operations.
- Do not shorten credentials, hashes, canonical object names, usernames, or
  externally defined IDs. Do not generate another ID for every log message.

# Work Guidance

- Keep the helper synchronous, bounded, and portable. It needs only Web Crypto.
- Never replace entropy failures with a constant ID.

# Verification

- Runtime Deno tests cover complete encoding, invalid prefixes and suffixes,
  type separation, and generation; registration tests own collision handling.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
