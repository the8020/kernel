# Purpose

- Own deployment cryptographic integrity and the platform JWT transport contract.

# Ownership

- Own the Ed25519 private key, atomic persistence/replacement, arbitrary-byte
  signing/verification, JWT issuance/verification, credential selection, and
  rejected-cookie removal. Never query application tables or depend on a database.
- Deno users owns accounts, password hashing, sessions, revocation, application
  cookie construction, and login/logout. Kernel execution principals are
  independent of all account rows, including system.

# Local Contracts

- `node/kernel/keys/signing.key` stores one standard base64-encoded 32-byte
  Ed25519 seed, mode 0600 beneath a mode-0700 directory. It is outside all
  service/job/development mounts, database storage, and the named secret store.
- Startup precedence: non-empty `THE8020_SIGNING_KEY` is validated and atomically
  persisted; otherwise load the file; otherwise generate once with crypto/rand.
  Invalid provisioning fails startup without exposing the value.
- `kernel.signing.replace` uses normal CBus secure input, atomically persists a
  replacement, and immediately activates it. A still-configured environment
  override wins again on the next restart. Status/replacement return only a
  SHA-256 public-key fingerprint. Never log or return private material.
- Nodes explicitly provisioned with the same seed accept the same tokens.
  Replacing it invalidates previous tokens immediately; there is no key ring,
  rotation grace period, external key lookup, or node-encryption scheme.
- Authentication JWT uses golang-jwt/v5, EdDSA with Ed25519 only, typ `the8020-auth+jwt`, kid
  equal to the current fingerprint, issuer `the8020`, and audience `the8020`.
  Require iat and exp, exp after iat, unexpired exp, iat not in the future, and
  valid nbf when present, without clock leeway. Tokens are at most 8192 bytes.
- Routing JWTs use the same deployment key/EdDSA/kid/issuer/audience with the
  distinct `the8020-route+jwt` type. Their only target fields are node, sandbox,
  Worker, and persistent execution IDs. They carry no service/user records or
  expiry lease: live supervisors alone govern keepalive and completion. Token
  verification proves integrity, never existence or permission to recreate an
  execution. Routing and authentication profiles reject each other's tokens.
- Claims require canonical `sub = user:<username>`, a nonempty opaque `sid` of
  at most 128 bytes, and a positive safe-integer `ver`. Only Deno interprets
  session existence, account state, and authentication-version eligibility.
  Cryptography cannot detect a revoked session or disabled account by itself.
- `the8020-authorization: Bearer <jwt>` and `the8020_auth=<jwt>` carry the same
  token. Explicit header presence wins, including empty, duplicate, or malformed
  headers; it never falls back to cookies. Duplicate platform cookies fail.
- Public services ignore tokens completely and forward credentials unverified
  under their configured user. Protected services verify before request-triggered
  execution and pass trusted claims to the existing target Worker for policy.
  Rejection uses existing service configuration and clears the selected rejected
  cookie with Path=/, HttpOnly, SameSite=Lax, and Secure on HTTPS.
- Trusted Deno services and jobs use the existing private operations bridge to
  sign arbitrary bytes and issue/verify JWTs. Raw signature validity never
  qualifies as HTTP authentication. There are no signing allowlists or special
  authentication sandboxes, Workers, services, or package copies.
- Peer credentials use their existing separate transport; node forwarding
  preserves the end-user platform header and cookie.

# Verification

- Tests cover private key persistence/modes/replacement, safe invalid input,
  DB-independent cross-node signatures and routes, strict JWT rejection, precedence, and
  cookie scope. HTTP/Worker and users-package regressions cover the policy split.

# Child DOX Index
