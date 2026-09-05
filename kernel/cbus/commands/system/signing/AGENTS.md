# Purpose

- Expose private deployment-key replacement and a public fingerprint through
  the existing command bus, independently of database/runtime initialization.

# Local Contracts

- Delegate key lifecycle to `kernel/auth.Signer`; never read, return, or log
  private key material. Replacement input uses the normal secure-input path.
- Replacement takes effect immediately and persists. A non-empty startup
  `THE8020_SIGNING_KEY` overrides the file on the next boot.

# Verification

- Key lifecycle tests and command-bus tests cover replacement, invalidation,
  safe failures, and availability before runtime initialization.
