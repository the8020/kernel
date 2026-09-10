Parent DOX: [sandbox DOX](../AGENTS.md).

# Purpose

- Build the one native runsc engine used by development, services, and jobs.

# Ownership

- Own the gVisor Gofer extension for persistent development workspaces and its
  pinned upstream SDK dependency. Development owns protected Git/publication
  transport; application packages own user workflows and presentation.

# Local Contracts

- `main.go` is ordinary production source. The extension activates only for
  kernel-prepared `the8020.workspace.*` annotations and the packages mount.
  Ordinary workload mounts keep the stock filesystem and seccomp rules.
- Execute the sentry compiled into this runsc. Build/install outputs link
  `gvisor-bin/gvisor_sentry` to `../runsc`; never ship a second stock engine
  that bypasses the SDK fixes. The named path also lets the upstream prewarmer
  invoke the correct executable.
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
- `main_test.go` checks ordinary mount and security-filter isolation. The
  development owner's `TestNativeSyscalls` runs identical syscall clients on
  host Linux and the workspace mount. `TestNativeWorkspace` verifies that
  unsupported metadata updates return their error without creating private
  edits. Native activation tests verify Git and process survival; portable
  runtime and service/job checks use the same common engine.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
