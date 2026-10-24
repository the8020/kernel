Parent DOX: [sandbox DOX](../AGENTS.md).

# Purpose

- Build the one native runsc engine used by development, services, and jobs.

# Ownership

- Own the gVisor Gofer extension for persistent development workspaces and its
  pinned upstream SDK dependency. Development owns protected Git/publication
  transport; application packages own user workflows and presentation.

# Local Contracts

- `main.go` is ordinary production source. The extension activates only for
  kernel-prepared `the8020.workspace.*` annotations and the packages/config
  mounts. Ordinary workload mounts keep the stock filesystem and seccomp rules.
- Execute the sentry compiled into this runsc. Build/install outputs link
  `gvisor-bin/gvisor_sentry` to `../runsc`; never ship a second stock engine
  that bypasses the SDK fixes. The named path also lets the upstream prewarmer
  invoke the correct executable.
- `deno.go` serves the read-only development config from its mounted template
  and the current merged package view. Enumerate only visible namespace/package
  directories and probe the three supported root config filenames; exclude
  hidden recovery/staging paths and directory aliases. Each open gets immutable
  bytes, while later opens/stat calls reflect membership and template updates.
  No background scanner, persisted inventory, source rewrite, or package-cache
  change is involved. The config mount alone disables shared open handles so a
  later reader cannot reuse an earlier membership snapshot. Ordinary Deno checks
  and LSP retain workspace/import-map behavior.
- Confine host operations to donated descriptors and `os.Root`. Shared lower
  files are read-only; private originals, edits, deletions, captures, and Git
  references persist under the development owner's workspace.
- Source mounts reject devices, sockets, allocation, extended attributes,
  unsupported metadata updates, and flagged renames. At most 4,096 changed paths
  are admitted per capture; directory enumeration materializes one directory.
  Broader filesystem and host-power-loss support requires separate
  qualification.
- Publication serializes only final filesystem checks and namespace changes.
  Hashing, Git, validation, and shared-source publication remain outside that
  lock. Bound changed-path and reference enumeration and metadata responses.
- `staging.go` supplies bounded metadata versions and retirement through the
  existing private control socket. Only exact `.conflicts/<attempt>/<package>`
  paths with ordinary parents qualify. Under the publication gate it checks
  every version and live inode reference, refuses external hardlinks, and
  atomically moves unchanged staging to protected capture storage. The
  development owner authorizes release only after its recovery obligations and
  non-forced Git registration removal. Small identity receipts make interrupted
  retirement/release retryable; arbitrary or legacy trees have no such
  authority.
- The private control owner's `discard` releases a rolled-back capture's payload
  without acknowledging source or changing publication heads. It checks capture
  path and parent identity, rejects published captures, clears only its own
  pending marker, and keeps small identity receipts for idempotent retry. Only
  the activation owner decides rollback and staging retirement are complete.
- `go.mod` pins the generated SDK for runtime release `20260817.0`. Update that
  pin with `defaults/config/runtime/versions.toml` and requalify native
  behavior.
- `sdk.patch` corrects dependency error and descriptor handling: propagate
  shared-file SetStat failures, return Linux `ENOENT` for an open removed
  directory, and read retained symlink descriptors with correct error
  propagation. Linux's
  [directory iterator](https://github.com/torvalds/linux/blob/master/fs/readdir.c)
  and
  [readlinkat contract](https://man7.org/linux/man-pages/man2/readlinkat.2.html)
  define the descriptor behavior. Apply corrections only to a disposable
  verified SDK copy; never mutate the module cache or rewrite project sources.
  Remove each correction when the pinned upstream dependency supplies it.
- The SDK mount-option correction permits read-only per-open handles on overlay
  hosts without adding the incompatible writable-handle promotion flag. Only the
  generated config requests this mode; writable package mounts retain their
  existing coherence settings. Remove the hunk when upstream supports this
  combination. `TestReadOnlyPerOpenConfigOnOverlayHost` checks this boundary.
- The same patch removes the upstream CLI's temporary embedded-sidecar import,
  omitting the unused cloud-checkpoint helper from the executable. Ordinary
  sandbox execution and local checkpoints retain their existing paths. Cloud
  checkpoints require the matching external helper; absence returns the upstream
  missing-sidecar error. Remove this hunk when upstream retires the embedding
  import.

# Work Guidance

- Keep only necessary native filesystem authority here. Tests and syscall
  clients must never be compiled into runsc's command dispatch.

# Verification

- `build.sh` verifies dependencies, applies the isolated SDK corrections, tests
  this module, and builds `.development/runtime-bin/runsc`. Docker builds
  (`THE8020_OUTER_CONTAINER_BUILD=true`) use `-s` to omit native symbol tables
  and DWARF, preserving the pinned version marker and Go stack traces. Ordinary
  development builds keep debugger information. The root build invokes it.
- `deno_test.go` checks explicit membership, private
  additions/deletions/renames, template updates, stable metadata and
  retained-reader snapshots. Development native Deno tests qualify the mount
  with the real Deno runtime.
- `TestDiscardCapturePreservesPrivateState` checks discard identity, retry,
  published-capture rejection and unchanged publication heads.
- `main_test.go` checks ordinary mount and security-filter isolation. The
  development owner's `TestNativeSyscalls` runs identical syscall clients on
  host Linux and the workspace mount. `TestNativeWorkspace` verifies that
  unsupported metadata updates return their error without creating private
  edits. Native activation tests verify Git and process survival; portable
  runtime and service/job checks use the same common engine.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
