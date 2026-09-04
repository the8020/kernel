# Purpose

- Own global named secrets used by kernel-authoritative operations.

# Ownership

- Persist secret values in `the8020__secrets__secrets`, expose explicit
  list/get/set operations, and resolve values for trusted kernel consumers.
- Do not own package metadata, Git behavior, authorization policy, command-bus
  transport, or application screens.

# Local Contracts

- Secret names use the platform-safe name grammar. Values are nonempty,
  bounded, and never included in list or set results.
- Database upserts serialize shared mutations across kernels. The database
  connection and credentials remain private to the kernel.
- Secret values are intentionally retrievable only by the explicit get method
  and trusted in-process resolvers. Callers must never log them.

# Work Guidance

- Do not add reversible encryption without a separately managed root key.

# Verification

- `store_test.go` covers empty stores, validation, overwrite behavior,
  concurrent serialization, durable reopening, and non-disclosing
  summaries.

# Child DOX Index
