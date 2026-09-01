# Purpose

- Hold tracked defaults installed into an 80|20 instance and the canonical
  generic runtime definition under `config/runtime/`.

# Ownership

- `config/` owns initial shared configuration templates; non-runtime operator
  files are installed only when absent.
- `config/runtime/` owns platform-maintained runtime versions, protocol source,
  Deno supervisor/Worker source, generic SDKs, and service/development image
  construction.
- `node/kernel/` owns the initial node-local settings template.
- `state/package-index/` owns the first-party desired package entries copied
  only when an instance's mapped package index has no entries.
- Defaults contain no generated node identity, secrets, user data, application
  package source, or materialized images.

# Local Contracts

- `install.sh`, not kernel startup, installs defaults. It atomically replaces
  the complete platform-owned `config/runtime/` tree so existing instances use
  current inputs and deleted runtime files cannot survive.
- Non-runtime shared configuration and node settings are created only when
  absent and are never overwritten on an existing instance.
- Package-index defaults declare public Git sources without credentials or
  version pins, so initial synchronization follows each repository's latest
  default-branch commit. Once any desired package entry exists, installation
  treats the complete mapped index as operator-owned and seeds nothing. A fresh
  instance synchronizes the seeded index once; subsequent installation and
  startup leave package updates to explicit synchronization.
- Materialized images, build caches, downloads, smoke records, and temporary
  construction output belong under `node/kernel/runtime/`, never in this tree
  or another top-level instance directory.
- Top-level template folders correspond only to initialized `config/`, `node/`,
  or `state/` roots. Application package source is independently owned and is
  cloned through the package index rather than copied from platform defaults.

# Work Guidance

- Keep operator defaults minimal and runtime inputs generic and package-neutral.

# Verification

- Installation and instance tests verify missing-only operator templates,
  empty-index seeding, runtime refresh, package/user roots, and node-local
  artifact placement.

# Child DOX Index

- `config/runtime/AGENTS.md`: canonical generic runtime source, image
  definitions, materialization, and verification.
