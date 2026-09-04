# Purpose

- Own ordinary users and opaque database-backed authentication sessions.

# Ownership

- Own Argon2id password hashing and PHC parsing, read-only user authentication,
  opaque authentication-cookie creation and validation, current-session
  logout, expiration cleanup, and trusted authentication context.
- Do not own user/session administration, HTTP service access policy, Deno
  transport, UUI logical sessions, browser rendering, roles, or permissions.

# Local Contracts

- Users and authentication sessions live only in
  `the8020__users__users` and `the8020__users__sessions`. All users are peers
  until a future permissions package defines roles. The package-owned
  `users.*` commands exclusively create and administer users and authentication
  sessions; the kernel has no parallel CRUD API or recovery-user commands.
- Zero users is a valid state. Construction verifies the users and
  authentication-session tables with bounded empty queries; missing or
  inaccessible tables block the service/UUI plane while the admin socket and
  database/package recovery remain available.
- Usernames are the shared Linux/storage/sandbox identity: 3-32 lowercase ASCII
  letters or digits, with no normalization, aliases, or path-safe conversion.
- Passwords use Argon2id with per-password random salts and encoded PHC parameters; unknown users follow the same Argon2 verification path as wrong passwords.
- Browser login may create an opaque authentication session. SSH password
  verification returns only trusted user context, accepts mutable password
  bytes without creating an immutable copy, and never retains or persists the
  presented secret. SSH public-key verification may resolve the same trusted
  context only for an existing enabled user after the protocol adapter verifies
  the separate key factor.
- Authentication cookies are opaque `v1.<id>.<secret>` values; the database
  contains only SHA-256 secret hashes and collision-safe inserts never overwrite
  an existing session.
- Package-owned database transactions serialize mutations across kernels.
  Password changes, disable operations, and explicit invalidation increment
  `auth_version`, which authentication observes on its next read.
- Ordinary validation is read-only except idempotent lazy deletion of expired
  records; every node may run cleanup concurrently without a global lock.
- Cookie headers are created completely by the kernel and always use `HttpOnly`,
  `Path=/`, and configured `SameSite`/`Secure` attributes.
- Active service dispatches register short-lived request identities and full
  kernel-only authentication context for authenticated supervisor callbacks;
  entries are removed when dispatch completes and are never persisted.

# Work Guidance

- Keep authentication-session and UUI-session terminology distinct. Never log
  or return password hashes, presented passwords, cookie secrets, or stored
  secret hashes.

# Verification

- Authentication and SSH tests cover PHC creation/parsing/verification, bounded
  table readiness, mutable transport-secret verification, package-owned user
  state changes, opaque session creation and cross-node validation, revocation,
  expiration, cleanup concurrency, cookie headers, password and public-key
  identity resolution, and secret non-disclosure.

# Child DOX Index
