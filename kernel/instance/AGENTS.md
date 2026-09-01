# Purpose

- Own one node installation's canonical paths, mapped shared roots, durable
  identity, initialization, Unix capability validation, process lock, and
  ephemeral control files.

# Ownership

- Own `node/kernel/paths.toml`, which maps independently synchronized
  `packages/`, `config/`, `state/`, and `users/` roots.
- Own creation of required shared directories, including private
  `config/secrets/`, `config/auth/`, `state/auth/bootstrap-sessions/`, `state/services/`,
  kernel-owned `state/package-index/`, and generic `state/package-data/`; own node-local settings, identity, bin,
  images, run/log/runtime/SSH roots, lock, PID, and command socket.
- Do not copy defaults, seed application packages, build images, discover parent
  projects, scan processes, or treat the PID file as a lock.

# Local Contracts

- Public API includes `Layout`, `Paths`, `ResolveRoot`, `PrepareLayout`,
  `WriteLayout`, `LoadPaths`, `CheckUnixPermissions`, `Initialize`,
  `Acquire`, and `Release`.
- Identity is a stable random UUID. Process locking uses non-blocking
  `unix.Flock`; stale PID/socket removal occurs only after lock acquisition.
- `Paths` exposes shared roots and node-local artifacts explicitly. Runtime
  versions come from `config/runtime/versions.toml`; service, development, and
  full image records/rootfs live beneath `node/kernel/runtime/images/`.
- Initialization canonicalizes mapped directories, rejects any overlap with
  `node/` or another mapped root, proves ownership/mode operations on a
  disposable file, creates empty package/user roots, and creates only empty
  settings files when absent.
- `state/package-index/` stores kernel-managed desired package documents.
- `config/secrets/secrets.toml` is the private global named-secret document;
  instance initialization creates only its mode-`0700` parent.
  `state/package-data/` is generic application data: instance code creates and
  mounts it but never enumerates or interprets package-owned contents.
- An explicit instance root may be absent. Resolution canonicalizes its nearest
  existing ancestor without creating it before the initialization decision.
- Private node/kernel directories are mode `0700`. The runtime attachments root
  alone is `0755` so it can be mounted read-only as `/artifacts`; bootstrap
  authentication sessions remain private.

# Work Guidance

- Keep path construction explicit and node-local artifacts beneath
  `node/kernel/`. Reject storage that cannot preserve the Unix ownership and
  mode operations required by sandbox package managers.

# Verification

- `instance_test.go` covers mapped roots, overlap and permission rejection,
  empty package/user/index/data roots, the private secret path,
  image/runtime paths, settings files, identity, modes, locking, stale cleanup,
  and release.

# Child DOX Index
