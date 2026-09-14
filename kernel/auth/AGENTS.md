Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own deployment cryptographic integrity, platform JWT transport, and native
  peer TLS credentials.

# Ownership

- Own the master seed, atomic persistence/replacement, derived Ed25519 signing
  credentials, arbitrary-byte signing/verification, JWT issuance/verification,
  credential selection, and rejected-cookie removal. Never query application
  tables or depend on a database.
- Deno users owns accounts, password hashing, sessions, revocation, application
  cookie construction, and login/logout. Kernel execution principals are
  independent of all account rows, including system.

# Local Contracts

- `node/kernel/keys/signing.key` stores one standard base64-encoded 32-byte
  random master seed, mode 0600 beneath a mode-0700 directory. It is outside all
  service/job/development mounts, database storage, and the named secret store.
- Startup precedence: non-empty `THE8020_SIGNING_KEY` is validated and
  atomically persisted; otherwise load the file; otherwise generate once with
  crypto/rand. Invalid provisioning fails startup without exposing the value.
- `kernel.signing.replace` uses normal CBus secure input, atomically persists a
  replacement, and immediately activates it. A still-configured environment
  override wins again on the next restart. Status/replacement return only a
  SHA-256 fingerprint of the derived authentication public key. Never log or
  return private material.
- The master is used only for HKDF-SHA256 derivation, never directly as a
  signing key. Use nil HKDF salt and `the8020/<purpose>/ed25519/v1` as `info`
  to derive each 32-byte Ed25519 seed. Fixed purposes are `app-session-cookie`
  for authentication JWTs, `service-routing` for route JWTs, and
  `node-forwarding` for peer TLS. Replacement publishes all derived keys
  together after successful persistence. No previous-key fallback exists.
- Nodes explicitly provisioned with the same seed accept the same tokens.
  Replacing it invalidates previous tokens immediately; there is no key ring,
  rotation grace period or external key lookup.
- Cluster deployment supplies the same `THE8020_SIGNING_KEY` to every node:
  standard base64 of one randomly generated 32-byte seed, provisioned once.
  Without the environment value or a persisted key, each node generates its
  own seed and does not automatically trust other nodes.
- `forwarding.go` uses the derived native-only peer key to create an in-memory
  certificate for mutual TLS 1.3. Both peers pin the current derived public
  key; public CAs, DNS certificates, application JWTs and arbitrary-byte
  signatures do not confer peer authority. No additional secret or certificate
  is persisted, mounted, or exposed through the package crypto bridge.
- Forwarding TLS callbacks use the current certificate after root replacement.
  Session tickets are disabled. The recipient revalidates every request against
  the current derived key, rejecting old pooled credentials and closing that
  connection; already-running streams finish. Provision replacements across all
  nodes together because different roots cannot authenticate each other.
- Authentication JWT uses golang-jwt/v5, EdDSA with Ed25519 only, typ
  `the8020-auth+jwt`, kid equal to the authentication key fingerprint, and
  issuer/audience `the8020`. Require iat and exp, exp after iat, unexpired exp,
  iat not in the future, and valid nbf when present, without clock leeway.
  Tokens are at most 8192 bytes.
- Routing JWTs use their own derived key and its public-key fingerprint as kid,
  with the same EdDSA/issuer/audience and distinct `the8020-route+jwt` type.
  Their only target fields are node, sandbox, Worker, and persistent execution
  IDs. They carry no service/user records or
  expiry lease: live supervisors alone govern keepalive and completion. Token
  verification proves integrity, never existence or permission to recreate an
  execution. Routing and authentication profiles reject each other's tokens.
- Route signing and verification require canonical `nod-`/`sbx-`/`wrk-`/`pex-`
  targets through the shared identity helper, including for correctly signed
  tokens. Opaque application authentication sessions keep their own contract.
- Claims require canonical `sub = user:<username>`. Session fields, including
  presence and shape of `sid` and `ver`, are opaque to Go and passed intact to
  Deno. Users owns their validation, session existence, account state, and
  authentication-version eligibility. Cryptography cannot detect a revoked
  session or disabled account by itself.
- Optional signed `transport` is `local` or `remote` (absence means remote).
  Local tokens require a native transport context; public HTTP, including
  loopback requests, cannot assert it. The authenticated node recipient alone
  may restore that marker when forwarding a native request across nodes.
- `the8020-authorization: Bearer <jwt>` and `the8020_auth=<jwt>` carry the same
  token. Explicit header presence wins, including empty, duplicate, or malformed
  headers; it never falls back to cookies. Duplicate platform cookies fail.
- Public services ignore tokens completely and forward credentials unverified
  under their configured user. Protected services verify before
  request-triggered execution and pass trusted claims to the existing target
  Worker for policy. Rejection uses existing service configuration and clears
  the selected rejected cookie with Path=/, HttpOnly, SameSite=Lax, and Secure
  on HTTPS.
- Trusted Deno services and jobs use the existing private operations bridge to
  sign arbitrary bytes with an explicit `app-` purpose and issue/verify JWTs.
  Generic signing and verification require 5–128 lowercase ASCII letters,
  digits, or hyphens, including the prefix and a nonempty suffix. Native Go
  enforces this boundary; verification uses the consumer's expected purpose.
  Arbitrary purposes are derived on demand without an unbounded cache.
  All trusted packages may use every application purpose, including
  `app-session-cookie`, and the dedicated JWT API; per-package permissions
  remain deferred. Native peer and routing keys are unavailable to generic
  signing. Raw signatures alone never qualify as HTTP authentication.
- Native peers use the derived key through the existing recipient transport;
  node forwarding preserves the end-user platform header and cookie.

# Work Guidance

- Treat cryptographic changes as protected kernel-foundation changes: establish
  necessity, keep the contract generic, and verify its package entrypoints.
  Account eligibility, login rules, and session policy remain in users; reuse
  the shared runtime rather than adding an authentication execution path.

# Verification

- Tests cover private key persistence/modes/replacement, safe invalid input,
  DB-independent cross-node signatures and routes, cross-purpose/master-key
  rejection, strict native JWT checks, opaque session claims, precedence, and
  cookie scope. HTTP/Worker and users-package regressions cover the policy split.
- `forwarding_test.go` verifies environment provisioning, restart precedence,
  separation from package signing, mutual peer authentication, and replacement
  on existing TLS configurations. Node tests verify pooled-connection rejection.
  Run `go test ./kernel/auth ./kernel/nodes` using the repository-local Go toolchain.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
