# Purpose

- Own private development workspaces, native durable storage, independent
  package Git repositories, and activation.

# Ownership

- Own development-workspace records and source trees under
  `users/<username>/workspaces/`, per-user `home/` and image-qualified `system/`
  roots, node-local runsc process metadata under
  `node/kernel/runtime/development/`, the workspace-scoped activation endpoint,
  mount-profile resolution, development-image delegation, and Git publication
  into shared package roots.
- Do not own service sandboxes, remote push, environment manifests,
  cross-package validation, activation history, or graphical programs.

# Local Contracts

- `/workspace/packages`, `/home/developer`, and the writable OCI root are direct
  mounts/roots backed by owning durable storage below the authenticated user's
  `users/<username>/` directory. Normal writes are durable immediately; stop,
  kill, restart, inherited-sandbox cleanup, kernel shutdown, and kernel startup
  inspect no file contents.
- Development system-root storage must preserve native Linux ownership, mode
  bits, symlinks, and atomic renames; metadata-emulating host shares are not a
  supported backing filesystem for interactive package installation.
- Rootless development launches every runsc lifecycle and exec process inside
  a kernel-created child user/mount namespace with the identity-mapped Linux
  UID/GID range `0..65535`. Native package-account ownership therefore reaches
  the durable system root without a filesystem shim or a broader host mount.
- Development lifecycle has no timer, autosave, draft format, scanner helper,
  rootfs tar, or gVisor overlay persistence. `node/kernel/` contains
  process/bundle metadata only and may be deleted without losing workspace
  files. Deleting a development sandbox removes its disposable bundle and
  process logs.
- Inherited runsc enumeration/deletion starts asynchronously and never gates
  manager or command-socket readiness. Persisted sandbox IDs are process-local:
  explicit workspace load lazily clears any ID not created by the current
  manager, without probing runsc or scanning all workspace records at startup.
- The authenticated user's default workspace ID is deterministic. SSH and other
  direct entrypoints ensure that one workspace by loading only its record, then
  creating or starting its sandbox as needed; this path never lists other
  workspaces. Ordinary path-safe usernames remain the storage owner ID; other
  valid external identities map to a stable hashed path-safe owner ID. Active
  development sandbox IDs map directly back to their owning workspace for
  console opens.
- New development sandboxes use the same canonical `sbx-` plus eight-character
  resource ID as every other sandbox. Exact current-manager ownership resolves
  an `sbx-` ID as development without a filesystem or runsc probe; legacy `dev-`
  IDs are accepted only while enumerating and deleting inherited runsc processes
  and are never newly allocated.
- The shared package tree is copied once when source storage is first created or
  explicitly reset. Each image-qualified system root is copied once from its
  immutable image. Copying streams through `cp -a --reflink=auto`, is staged
  atomically, publishes the system root itself as traversable mode `0755`, and
  retains bounded diagnostics. The empty `/proc` and `/sys` image placeholders
  are created as ordinary writable mount points instead of preserving immutable
  template modes that shared filesystems may reject.
- A workspace source is scanned only by Git during an explicit
  `development activate preview` or `development activate run`. A temporary Git
  index compares each private working tree with its recorded base without
  serializing file contents through Go or changing the private index.
- Shared repository inspection uses a repository lock independent of workspace
  lifecycle. Activation scanning occurs before the publication lock; only the
  bounded shared Git prepare/publish window excludes repository mutation. The
  same lock excludes package-manager index/source synchronization so activation
  and remote replacement cannot mutate one shared repository concurrently.
- Activation creates one commit per changed selected package, uses Git
  cherry-pick conflict machinery, never pushes, preserves unselected packages,
  and synchronizes successfully activated private package directories in place.
  It pauses the sandbox only for a stable explicit operation and never recreates
  it. Conflict markers are written directly into the durable private source.
- Source reset replaces only workspace source and recorded bases. Factory reset
  additionally replaces developer home and all image-qualified system roots but
  preserves unrelated data in the owning user directory. Both require explicit
  confirmation.
- The direct runsc driver uses the selected rootful/rootless mode, a writable
  private OCI root, direct writable source/home bind mounts, validated shared
  read-only mounts, ephemeral tmpfs mounts, and read-only mode-`0644` resolver
  files so package-manager service accounts retain DNS access. It configures no
  runsc overlay or rootfs-tar annotation; `--overlay2=none` is explicit because
  runsc otherwise defaults the root to an ephemeral self-backed overlay.
- Development exec and all kernel-owned subprocess diagnostics are bounded to
  one MiB. Large output is truncated before it can enter errors or logs.
- The helper endpoint authenticates one workspace token, fixes the helper client
  identity, and re-enters the registered activation command handlers; it is not
  a second activation implementation.
- Development console and direct-stream exec processes retain the bounded
  package-management capabilities required for APT/dpkg, keep
  `no_new_privileges`, omit broad host-adjacent capabilities, include the
  standard administrative `sbin` directories in `PATH`, and remain inside
  gVisor.

# Work Guidance

- Prefer deleting lifecycle machinery over reconciling derivable state. Never
  introduce a filesystem polling loop, full-tree serialization, or background
  scan when native durable storage or an explicit operation owns the need.
- Never hold the workspace mutex for package repository inspection, and never
  put unbounded subprocess output in an error.

# Verification

- Unit tests prove zero idle sandbox work, native source/home/system persistence
  through process replacement, developer isolation, explicit-only Git scans,
  activation without sandbox recreation, persisted conflicts, independent
  repository inspection, non-blocking inherited cleanup/lazy identity
  normalization, reset boundaries, bounded diagnostics, and absence of runsc
  overlay/tar flags, rootless package-account UID/GID mapping, canonical sandbox
  IDs and legacy inherited cleanup, plus direct default-workspace
  ensure/reuse/restart.
- The rootless and rootful real-gVisor E2E proves SSH password login and PTY
  control/resize behavior, a contextual prompt that follows `cd`, and a real
  `xterm` Nano full-screen session, native non-root UID/GID ownership, ordinary
  edits plus APT/dpkg installation survive restart natively, helper
  preview/activation works, source reset retains home/system state, factory
  reset removes it, PID 1 remains `sleep`, deleted process logs do not
  accumulate, and no scanner executable exists in the image.

# Child DOX Index
