# Purpose

- Own global named secrets used by kernel-authoritative operations.

# Ownership

- Persist secret values in `config/secrets/secrets.toml`, expose explicit
  list/get/set operations, and resolve values for trusted kernel consumers.
- Do not own package metadata, Git behavior, authorization policy, command-bus
  transport, or application screens.

# Local Contracts

- Secret names use the platform-safe name grammar. Values are nonempty,
  bounded, stored only in the mode-`0600` TOML document, and never included in
  list or set results.
- Mutations use a mode-`0600` advisory lock and atomic file replacement so
  kernels sharing the global configuration root cannot partially overwrite one
  another.
- Secret values are intentionally retrievable only by the explicit get method
  and trusted in-process resolvers. Callers must never log them.

# Work Guidance

- Do not add reversible encryption without a separately managed root key.

# Verification

- `store_test.go` covers empty stores, validation, overwrite behavior,
  cross-instance serialization, persistence, file modes, and non-disclosing
  summaries.

# Child DOX Index
