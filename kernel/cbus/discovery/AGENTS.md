# Purpose

- Discover package-owned command manifests and atomically publish their live
  command-bus registrations.

# Local Contracts

- Only ready active packages are indexed. TOML on the shared package mount is
  the source of truth; assembled catalogs are process-local and non-durable.
- Command names derive from package identity and nested directories. First-party
  `the8020` names omit that namespace; third-party names retain it.
- Package fragments are validated independently. A broken fragment is omitted
  without hiding valid packages.
- Candidate command manifests and their same-package programs are validated
  before package activation switches source.
- Package programs may return kernel-originated structured command failures;
  the registry preserves their code/message/details instead of flattening them
  into a generic runtime error.

# Verification

- Tests cover names, nesting, malformed manifests, reserved names, duplicates,
  active-package filtering, candidate validation, and atomic refresh.

# Child DOX Index
