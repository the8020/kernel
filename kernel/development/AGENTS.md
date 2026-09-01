# Purpose

- Own each user's single persistent development sandbox, package-level Git
  activation, and the independent development image/runtime.

# Ownership

- Own the record and every durable sandbox artifact beneath
  `users/<username>/dev-sandbox/`, disposable runsc metadata beneath
  `node/kernel/runtime/development/`, mount-profile resolution, the authenticated
  activation endpoint, and publication into shared package repositories.
- Do not own service/job sandboxes, remote push, package manifests,
  cross-package validation, activation history, or graphical programs.

# Local Contracts

- The authenticated lowercase alphanumeric `user_id` is the only control-plane
  key. Authentication guarantees 3-32 characters. The runtime ID is exactly
  `dev-<user_id>`; there are no workspace IDs, aliases, hashes, conversions, or
  compatibility paths.
- Durable state is confined to `users/<username>/dev-sandbox/`: `sandbox.toml`,
  overlay checkpoints, and image-qualified writable system
  roots. Unrelated files beneath `users/<username>/` are not sandbox state.
- `/workspace/packages` is a gVisor-private writable overlay over the shared
  package tree. The live gVisor filestore is disposable; explicit lifecycle
  boundaries checkpoint private package deltas beneath `dev-sandbox/runtime/`
  and restore them on start. There is no timer, autosave loop, filesystem
  scanner, full-tree copy, or serialized file-content format.
- The writable OCI system root, including `/root`, is initialized from the
  current development image only when the sandbox is first created or after a
  confirmed factory reset. Its recorded image-qualified path and image
  provenance are then retained across image changes and ordinary lifecycle
  operations. Missing, unsafe, or inconsistent recorded roots fail closed and
  require explicit recovery or factory reset. Native Linux ownership, modes,
  symlinks, and atomic renames are preserved; `/run` and `/tmp` remain
  tmpfs-backed and intentionally unpersisted.
- Authorized-key lookup is a read-only operation on an already initialized
  sandbox. It reads only the bounded regular `/root/.ssh/authorized_keys` file
  beneath the record's expected canonical system root, rejects symlinks and
  malformed roots, and never creates, starts, restarts, or mutates a sandbox.
- Development sandboxes run as Linux root and have no `developer` account or
  `/home/developer`. Rootless runsc processes use the kernel-created identity
  mapping for Linux UID/GID `0..65535`.
- Manager startup never waits for inherited runsc cleanup or scans sandbox
  records. User lifecycle calls load only
  `users/<user_id>/dev-sandbox/sandbox.toml`; list is the sole operation that
  enumerates users. Per-user and per-runtime-ID locks serialize lifecycle and
  inherited cleanup for deterministic IDs.
- Git scans happen only during explicit activation preview/run or lifecycle
  checkpointing. Activation creates one commit per selected changed package,
  uses Git merge/cherry-pick machinery, never pushes, preserves unselected
  changes, and recreates the same deterministic sandbox with a clean overlay.
- Preview always returns an array, including `packages = []` after reset. It
  reports every changed Git package with file and added/removed-row counts;
  changes remain visible but blocked when the shared worktree is not clean and
  activation-ready.
- A nonblank message is required for publication. The authenticated username is
  the default Git author name and email stem, and each package commit ends with
  a valid `[the8020.activation]` TOML appendix containing the sandbox identity
  and sanitized technical metadata.
- Repository locking is independent of the per-user lifecycle lock. Never hold
  the lifecycle lock merely to inspect a shared repository.
- Source reset discards overlay changes and recorded bases while preserving the
  recorded system root and image provenance. Factory reset is the sole path
  that removes exactly `users/<user>/dev-sandbox/` and initializes a replacement
  root from the current development image, preserving unrelated user data. Both
  require confirmation.
- The helper endpoint authenticates the sandbox token, fixes helper client
  metadata, and re-enters registered activation commands; it is not a second
  activation implementation.
- The platform-owned instance `scripts/` tree is mounted read-only and
  executable at `/workspace/scripts`; `/workspace/scripts/activate` is the
  canonical terminal helper and remains outside the mutable image system root.
- Exec and subprocess diagnostics are bounded to one MiB. Development consoles
  retain the bounded capabilities required for APT/dpkg, use the standard
  administrative `PATH`, keep `no_new_privileges`, and remain inside gVisor.

# Work Guidance

- Prefer derivable state and one direct per-user path over registries, aliases,
  migrations, background reconciliation, or per-file persistence exceptions.
- Keep temporary runtime files temporary and durable sandbox files beneath the
  one `dev-sandbox` root.

# Verification

- Unit tests cover deterministic IDs, direct ensure/reuse/restart, bounded and
  confined authorized-key reads without lifecycle mutation, user
  isolation, overlay checkpoint/restore, explicit Git scans, activation/reset
  boundaries, independent repository inspection, inherited-cleanup races,
  bounded diagnostics, and OCI mount policy.
- The real gVisor E2E covers SSH/PTY behavior, APT/dpkg persistence across
  restart, temporary `/run`, read-only helper mounting, repeated helper
  activation and clean overlay resets, source and factory reset, root identity,
  and absence of a developer account.
