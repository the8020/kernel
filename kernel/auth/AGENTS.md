# Purpose

- Own the bootstrap-administrator realm and shared file-backed authentication sessions used before database identity exists.

# Ownership

- Own Argon2id password hashing and PHC parsing, atomic bootstrap-user configuration, opaque authentication-cookie creation and validation, immutable shared authentication-session files, revocation, expiration cleanup, and trusted bootstrap auth context.
- Do not own HTTP service access policy, Deno transport, UUI logical sessions, browser rendering, roles, or permissions.

# Local Contracts

- Bootstrap users live only in `config/auth/bootstrap-users.toml`; session records live only below sharded `state/auth/bootstrap-sessions/` and neither tree is sandbox-mounted.
- Passwords use Argon2id with per-password random salts and encoded PHC parameters; unknown users follow the same Argon2 verification path as wrong passwords.
- Browser login may create an opaque authentication session; SSH password
  verification returns only trusted user context, accepts mutable password
  bytes without creating an immutable copy, and never retains or persists the
  presented secret.
- Authentication cookies are opaque `v1.<id>.<secret>` values; files contain only SHA-256 secret hashes and are published atomically without overwriting collisions.
- User mutations use one advisory lock and restrictive atomic replacement. Password changes, disable operations, and explicit invalidation increment `auth_version`.
- Ordinary validation is read-only except idempotent lazy deletion of expired records; every node may run cleanup concurrently without a global lock.
- Cookie headers are created completely by the kernel and always use `HttpOnly`, `Path=/`, and configured `SameSite`/`Secure` attributes.
- Active service dispatches register short-lived request identities and full kernel-only authentication context for authenticated supervisor callbacks; entries are removed when dispatch completes and are never persisted.

# Work Guidance

- Keep authentication-session and UUI-session terminology distinct. Never log or return password hashes, presented passwords, cookie secrets, or stored secret hashes through administrative summaries.

# Verification

- Package and SSH integration tests cover PHC creation/parsing/verification,
  mutable transport-secret verification, atomic and cross-process user
  mutation, corruption handling, opaque session creation and cross-node
  validation, revocation, expiration, cleanup concurrency, cookie headers,
  disabled/version-invalid users, and secret non-disclosure.

# Child DOX Index
