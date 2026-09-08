Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own each user's single persistent development sandbox, package-level Git
  activation, and the independent development image/runtime.

# Ownership

- Own the record and every durable sandbox artifact beneath
  `users/<username>/dev-sandbox/`, disposable runsc metadata beneath
  `node/kernel/runtime/development/`, mount-profile resolution, the
  authenticated activation endpoint, and publication into shared package
  repositories.
- Do not own service/job sandboxes, remote repository administration, package
  manifests, cross-package validation, activation history, or graphical
  programs.

# Local Contracts

## Requested workflow

- Implement and verify persistent native terminals before filesystem/Git work.
  Phase 2 starts only in disposable checkouts and instances; production adoption
  requires demonstrated correctness, ordinary native-tool behavior, and low
  overhead. Keep the completed terminal feature if no filesystem candidate
  qualifies. Stop after Phase 1 is finished and verified. The user will revise
  Phase 2 before authorizing its start; do not begin its experiments or
  implementation automatically.
  [WORKFLOW_IMPLEMENTATION.md](WORKFLOW_IMPLEMENTATION.md) tracks the ordered
  implementation and verification gates.
- The mounted workspace must support ordinary Linux filesystem behavior for
  unmodified Git, Codex, Claude Code, editors, and build tools. Applications
  must use normal paths and filesystem operations without knowing whether a file
  is shared or private. Validate executable loading, mmap, locks, atomic saves,
  links, open-handle behavior, and file watching; ordinary read/write tests
  alone do not qualify a backend. Persistence bookkeeping stays below this
  interface.
- Activation must preserve running sandbox processes. Named terminal sessions
  must survive navigation, refresh, switching views, and logout, with multiple
  sessions selectable in the development-test program. Explicit session close
  and sandbox shutdown end them; kernel-owned idle deadlines also apply.
- Use unmodified htop for Phase 1 interactive compatibility checks: actual
  rendering, keyboard shortcuts, scrolling, resizing, and recovery across
  disconnects. The user waived separate Codex/Claude Code interface tests.
  Retain the terminal protocol, input/paste, lifetime, and performance checks.
- Prefer reusing the shared native console/PTY boundary beneath SSH and UUI for
  persistent terminals. Give the PTY a lifetime independent of client sockets;
  reconnect reuses a live process and a named open recreates a missing one.
  Kernel owns the sandbox-scoped session name and physical terminal, and Deno
  packages own selection, workflow, and display recovery. The shared retained
  owner and browser/SSH adapters are implemented and verified; the checklist
  records the completed Phase 1 evidence and the Phase 2 hold.
- Untouched paths follow shared publication immediately; private edits stay
  isolated. Git must merge from each path's original observed version, preserve
  non-overlapping changes, and reject real conflicts with a nonzero helper exit.
  Conflicting paths, originals, both sides, and applicable text markers must
  remain available inside the workspace for resolution and retry.
- Private source must survive runtime loss and transfer to another sandbox as
  readable/reapplicable state without periodic scanning or autosave. Publication
  must preserve later writes, avoid long shared locks, and never reset the
  sandbox to clear an overlay.
- Reliability and low system overhead are acceptance gates. Benchmark actual
  native-tool filesystem operations and concurrent developer activity against
  the existing backend; fast Git candidate preparation alone does not qualify
  workspace performance. Bound terminal history and replay work independently.
- [analysis/REPORT.md](analysis/REPORT.md) records measured failures, design
  recommendations, and filesystem qualification gates. The filesystem redesign
  is pending; current activation still recreates the sandbox and does not meet
  the process-preservation and publication requirements above.

## Current implementation

- The authenticated lowercase alphanumeric `user_id` is the only control-plane
  key. The shared kernel principal contract guarantees 3-32 characters. The
  sandbox has an opaque `sbx-` ID from the shared identity helper, persisted in
  schema-2 `sandbox.toml`. Ordinary restart and activation retain the ID;
  deletion or factory reset ends it. Username ownership and storage never derive
  from the opaque ID. Creation serializes registration and rejects collisions
  against retained development records; live ownership checks reject foreign
  reuse before any backend cleanup.
- Durable state is confined to `users/<username>/dev-sandbox/`: `sandbox.toml`,
  overlay checkpoints, and image-qualified writable system roots. Unrelated
  files beneath `users/<username>/` are not sandbox state.
- `/workspace/packages` is a gVisor-private writable overlay over the shared
  package tree. The live gVisor filestore is disposable; explicit lifecycle
  boundaries checkpoint private package deltas beneath `dev-sandbox/runtime/`
  and restore them on start. There is no periodic checkpoint timer, autosave
  loop, filesystem scanner, full-tree copy, or serialized file-content format.
- Node-local `development.idle_timeout` defaults to two hours with no ordinary
  consoles, pending opens, retained terminals, or active shell commands. A
  detached or exited retained terminal still protects its sandbox until the
  terminal is destroyed. The last release starts the interval; new admission
  cancels it. An unused newly started sandbox is idle immediately.
- The active ownership index holds per-sandbox usage and one idle deadline;
  timers never enumerate user records. Admission and idle stop share the
  per-user lifecycle lock, and old releases cannot affect replacement sandboxes.
  Runtime setting changes retain each original idle start. Expiry uses ordinary
  checkpoint/stop; checkpoint failure preserves the sandbox, logs the failure,
  and retries after another idle interval. No application package owns a timer.
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
  mapping for Linux UID/GID `0..65535`. Runsc directfs avoids gofer round trips
  while retaining gVisor's exact donated-mount boundary and the private package
  overlay.
- Manager startup never waits for inherited runsc cleanup or scans sandbox
  records. User lifecycle calls load only
  `users/<user_id>/dev-sandbox/sandbox.toml`. Only listing and cold identity
  registration enumerate user records. Per-user and per-sandbox-ID locks
  serialize lifecycle and inherited cleanup. The console broker resolves active
  development targets through `HasSandbox`, never by decoding a username.
- Git scans happen only during explicit activation preview/run or lifecycle
  checkpointing. Activation creates one commit per selected changed package,
  uses Git merge/cherry-pick machinery, never pushes, preserves unselected
  changes, and recreates the same registered sandbox with a clean overlay. Local
  edits never affect the database. After candidate commits are staged, the
  shared schema deployment hook validates and synchronizes affected tables
  before Git references/source are published; failure leaves shared code and
  unrelated private changes intact.
- One kernel-owned non-login sandbox command scans all initialized package
  repositories in a preview or activation. Disposable per-package indexes live
  only in the sandbox's `/tmp`, reset when the shared base changes or an index
  is invalid, and refresh incrementally across scans; patch capture reads the
  exact index produced by its scan instead of rebuilding the package tree. New
  untracked files excluded by the repository's standard Git ignore rules are
  never previewed, checkpointed, or activated; already tracked paths retain
  normal Git modification and deletion behavior even if later ignore rules match
  them. Preview computes detailed raw and line statistics; activation and
  lifecycle capture use a cheap changed/not-changed comparison before exporting
  patches because those paths do not return file statistics.
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
  recorded system root and image provenance. Factory reset is the sole path that
  removes exactly `users/<user>/dev-sandbox/` and initializes a replacement root
  from the current development image, preserving unrelated user data. Both
  require confirmation.
- The helper endpoint authenticates the sandbox token, fixes helper client
  metadata, and re-enters registered activation commands; it is not a second
  activation implementation.
- The platform-owned instance `scripts/` tree is mounted read-only and
  executable at `/workspace/scripts`; `/workspace/scripts/activate` is the
  canonical terminal helper and remains outside the mutable image system root.
  `install-codex.sh` and `install-claude.sh` opt in to the vendors' latest
  native releases, persist them in root's home, and set only unattended
  full-access permissions. Root's native user-binary directory is on every
  development command PATH.
- Exec and subprocess diagnostics are bounded to one MiB. Development consoles
  retain the bounded capabilities required for APT/dpkg, use the standard
  administrative `PATH`, keep `no_new_privileges`, and remain inside gVisor.

# Work Guidance

- Prefer derivable state and one direct per-user path over registries, aliases,
  migrations, background reconciliation, or per-file persistence exceptions.
- Keep temporary runtime files temporary and durable sandbox files beneath the
  one `dev-sandbox` root.
- `ACTIVATION_PERFORMANCE.md` records the representative scan matrix and the
  evidence behind retained and rejected activation optimizations.

# Verification

- Unit tests cover opaque IDs, collision rejection, direct ensure/reuse/restart,
  bounded and confined authorized-key reads without lifecycle mutation, user
  isolation, overlay checkpoint/restore, explicit batched Git scans,
  ignored-artifact exclusion, disposable-index recovery, schema-before-source
  activation and rollback, activation/reset boundaries, repository-lock
  separation, inherited-cleanup races, bounded diagnostics, and OCI mount
  policy.
- The real gVisor E2E covers SSH/PTY behavior, APT/dpkg persistence across
  restart, temporary `/run`, directfs package isolation, ignored-artifact
  exclusion, read-only helper mounting, repeated helper activation and clean
  overlay resets, source and factory reset, root identity, and absence of a
  developer account.
- The opt-in `TestRootlessRetainedTerminals` uses disposable native gVisor PTYs
  to check two independent shells across detach/reattach/switch, detached
  output, explicit close, and process exit. Enable `THE8020_TERMINAL_E2E=1`. It
  does not replace browser or htop display qualification.
- Short-timeout unit tests cover last-console release, reconnect, running shell
  protection, checkpoint failure, and private-file restoration. The dev-core
  `test:native-idle` harness checks the real browser/SSH/metadata path with
  seconds-long deadlines and subsequent sandbox shutdown.

# Child DOX Index

- [analysis/AGENTS.md](analysis/AGENTS.md): disposable workflow experiments,
  measurements, and recommendations; production behavior stays owned here.
