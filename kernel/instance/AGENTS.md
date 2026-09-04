# Purpose

- Own one node installation's fixed filesystem layout, durable node identity,
  initialization, Unix capability validation, process lock, and control files.

# Ownership

- Own `<instance>/kernel.toml`, fixed `packages/`, `users/`, `database/`,
  `scripts/`, and `node/` roots, and node-local bin, image, runtime, log, SSH,
  lock, PID, and command-socket paths.
- Do not own shared configuration or operational state; those are database
  tables. Do not copy defaults, stage packages, build images, discover parent
  projects, scan processes, or treat the PID file as a lock.

# Local Contracts

- Public API includes `Paths`, `ResolveRoot`, `Prepare`, `LoadPaths`,
  `CheckUnixPermissions`, `Initialize`, `Acquire`, and `Release`.
- Identity is a stable random UUID stored in `kernel.toml`. The same file stores
  every persisted per-node kernel setting; it is the only node configuration
  file.
- Process locking uses non-blocking `unix.Flock`; stale PID/socket removal
  occurs only after lock acquisition.
- Initialization uses the fixed layout, proves ownership/mode operations on a
  disposable file, creates empty package/user roots, and never creates legacy
  top-level `config/` or `state/` roots.
- Runtime definitions and materialized images live below
  `node/kernel/runtime/`; the private `database/` directory owns the default
  single-node SQLite file and is never sandbox-mounted.
- An explicit instance root may be absent. Resolution canonicalizes its nearest
  existing ancestor without creating it before the initialization decision.
- Private node/kernel and database directories are mode `0700`; the SQLite file
  is mode `0600`. Runtime attachments alone are `0755` for read-only sandbox
  mounting.

# Work Guidance

- Keep path construction explicit and node-local artifacts beneath
  `node/kernel/`. Reject storage that cannot preserve Unix ownership and modes.

# Verification

- `instance_test.go` covers fixed roots, absence of legacy roots, identity,
  settings in `kernel.toml`, modes, permission checks, locking, stale cleanup,
  and release.

# Child DOX Index
