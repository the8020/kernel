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

- Production implementations live in ordinary source. Builds never extract test
  code, rewrite project source, or retain replaced backends for comparison.
- Native tests assert behavior directly. Keep diagnostics for failures and
  resource assertions; do not retain prototype report builders or result files.
- Keep this native owner limited to sandbox authority, protected filesystem/Git
  operations, durable publication/recovery, and process lifetime. Deno packages
  own command presentation, screens, and user workflows.
- Untouched files follow shared publication immediately; private edits remain
  isolated. Merge from each path's original observed version, preserve later
  edits, and retain native conflict worktrees for CLI/UUI continuation. Retained
  originals keep upstream-deleted packages eligible for conflict recovery even
  before the developer's private Git repository has a commit.
- Small edits must not copy whole repositories or asset histories. Ordinary
  tools use ordinary paths, without an explicit package-editing gate. Private
  source survives runtime loss without periodic scanning or autosave.
- Publication preserves running processes and named terminals. Explicit close,
  sandbox shutdown, and kernel-owned idle deadlines govern their lifetimes.
- Keep native filesystem checks for Git, atomic saves, links, executable
  loading, mmap, retained handles, and private Git object retention. Broader
  filesystem compatibility and host-power-loss qualification remain separate
  gates.

- The authenticated lowercase alphanumeric `user_id` is the only control-plane
  key. The shared kernel principal contract guarantees 3-32 characters. The
  sandbox has an opaque `sbx-` ID from the shared identity helper, persisted in
  schema-2 `sandbox.toml`. Ordinary restart and activation retain the ID;
  deletion or factory reset ends it. Username ownership and storage never derive
  from the opaque ID. Creation serializes registration and rejects collisions
  against retained development records; live ownership checks reject foreign
  reuse before any backend cleanup.
- Durable state is confined to `users/<username>/dev-sandbox/`: `sandbox.toml`,
  sparse workspace data, private `skills/`, and image-qualified writable system
  roots. Unrelated files beneath `users/<username>/` are not sandbox state.
- Development uses the same installed runsc as ordinary workloads, selected by
  the existing rootless/full runtime configuration. Only development mounts
  enable the sparse Gofer and its additional host syscall allowances.
- `/workspace/packages` uses the sparse Gofer over the live shared tree. Private
  files, originals, deletion records, captures and native Git state live beneath
  `dev-sandbox/workspace/` and survive sandbox loss. Activation acknowledges its
  captured changes without restarting processes or discarding later edits. There
  is no periodic checkpoint timer, autosave loop or full-tree copy. A legacy
  checkpoint containing private edits blocks startup with instructions to
  activate or export those edits using the previous kernel; never silently
  ignore that work when switching backends.
- Node-local `development.idle_timeout` defaults to two hours with no ordinary
  consoles, pending opens, retained terminals, or active shell commands. A
  detached or exited retained terminal still protects its sandbox until the
  terminal is destroyed. The last release starts the interval; new admission
  cancels it. An unused newly started sandbox is idle immediately.
- The active ownership index holds per-sandbox usage and one idle deadline;
  timers never enumerate user records. Admission and idle stop share the
  per-user lifecycle lock, and old releases cannot affect replacement sandboxes.
  Runtime setting changes retain each original idle start. Expiry uses ordinary
  stop; failure preserves ownership, logs the failure, and retries after another
  idle interval. Private files are already persistent. No application package
  owns a timer.
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
  mapping for Linux UID/GID `0..65535`. The development runsc uses the sparse
  Gofer with directfs disabled, retaining the exact donated-mount boundary.
- Manager startup never waits for inherited runsc cleanup or scans sandbox
  records. User lifecycle calls load only
  `users/<user_id>/dev-sandbox/sandbox.toml`. Only listing and cold identity
  registration enumerate user records. Per-user and per-sandbox-ID locks
  serialize lifecycle and inherited cleanup. The console broker resolves active
  development targets through `HasSandbox`, never by decoding a username.
- Preview and activation enumerate private changes and prepare native Git only
  for selected packages. Activation uses each path's retained original for
  merging, never pushes, and preserves unselected changes and running processes.
  Local edits never affect the database. The shared transaction validates and
  synchronizes candidates before publication. Native conflict worktrees retain
  failed attempts for either the CLI or UUI to resolve and continue.
  `workspace.go` owns native workspace/Git transport; `activation.go` owns
  capture, candidate preparation, publication, and journal recovery.
- Preview always returns an array, including `packages = []` after reset. It
  reports every changed Git package with file and added/removed-row counts;
  changes remain visible but blocked when the shared worktree is not clean and
  activation-ready.
- Preview uses `added`, `modified`, and `deleted` consistently for package and
  file changes. File/directory moves appear as deletions and additions, matching
  the selected-file diff. This presentation does not disable Git's merge rename
  detection. Package and namespace moves publish both ends together; new package
  folders need a manifest, and activation initializes missing Git metadata.
- Optional `preview_file` with one selected package loads only that changed
  file's bounded `diff: {text, notice?}`. Ordinary previews omit contents. Text
  uses native Git hunks without filesystem headers; binary, metadata-only, and
  truncated output have explicit notices. Private source and the Git index used
  for ordinary development are never edited by review.
- A nonblank message is required for publication. The authenticated username is
  the default Git author name and email stem, and each package commit ends with
  a valid `[the8020.activation]` TOML appendix containing the sandbox identity
  and sanitized technical metadata.
- Repository locking is independent of the per-user lifecycle lock. Never hold
  the lifecycle lock merely to inspect a shared repository.
- Source reset discards private workspace changes and recorded bases while
  preserving the recorded system root and image provenance. Factory reset is the
  sole path that removes exactly `users/<user>/dev-sandbox/` and initializes a
  replacement root from the current development image, preserving unrelated user
  data. Both require confirmation.
- The helper endpoint authenticates the sandbox token, fixes helper client
  metadata, resolves `dev-core.activate.preview` / `dev-core.activate.run` from
  the current command catalog, and passes ordinary package-command arguments. It
  is not a second activation implementation.
- Activation results retain a top-level error and, for resumable native
  conflicts, a `conflict_worktree` per package. The UUI uses sandbox inspection
  and the platform's native Git adapter to reopen that exact attempt; terminal
  output defaults to readable instructions, with `--json` for integrations.
- The platform-owned instance `scripts/` tree is mounted read-only and
  executable at `/workspace/scripts`; `/workspace/scripts/activate` is the
  canonical terminal helper and remains outside the mutable image system root.
  `install-codex.sh` and `install-claude.sh` opt in to the vendors' latest
  native releases, persist them in root's home, and set only unattended
  full-access permissions. Installers register `/usr/local/bin` symlinks so
  browser and SSH shells can invoke both tools immediately even when their PATH
  overrides omit root's native user-binary directory.
- The activated `packages/the8020/dev-skills` package is mounted read-only at
  `/workspace/skills/builtin`; its `workspace.md` also supplies
  `/workspace/AGENTS.md` and `/workspace/CLAUDE.md`. Skills update through
  ordinary package activation. Always use `8020-dev`, then its relevant domain
  skills.
- `Config.SystemURL` supplies the node's main HTTP URL at each sandbox start.
  The driver exposes it as `DEVELOPMENT_SYSTEM_URL`, separately from the private
  activation endpoint and token. Its loopback address uses development's host
  network; an existing process retains its start-time value if the main port
  changes. Environment explanations remain in dev-skills `workspace.md`.
- The existing private sandbox ingress also exposes `token` and `request`.
  Authenticate its sandbox secret before passing the loaded record's owner to
  composition. The body cannot select a user. The existing bearer secret is
  transferable within the host network; Unix peer identity is not claimed.
  Composition issues an ordinary users allowance and dispatches generic service
  requests with native transport provenance; no UUI events enter this package.
- `/workspace/skills/custom` is a writable persistent mount backed by
  `users/<user-id>/dev-sandbox/skills/`. It survives restart, activation, and
  source reset; factory reset removes it with the rest of that user's sandbox.
  The package-owned discovery helper merges both sources into native agent home
  directories, preferring custom names and preserving unmanaged home skills.
  Startup, CLI installation, or an explicit
  `/workspace/scripts/setup-agent-skills.sh` refresh discovers
  additions/removals; agents may need a catalog reload.
- Read-only mount sources may be directories or regular files, with the same
  canonical-path confinement and private-root exclusions. Writable source and
  persistent mounts remain directories. The mounted
  `/workspace/scripts/development-init.sh` installs agent discovery in the
  retained home, then delegates to the image's existing `sandbox.sh`. Custom
  driver profiles must supply that bootstrap and its built-in/custom mounts too.
- Running sandboxes retain their mount bindings until their next ordinary start;
  replacing a mounted instruction file becomes visible on that start. The
  built-in directory exposes published skill updates through normal file reads.
- Exec and subprocess diagnostics are bounded to one MiB. Development consoles
  retain the bounded capabilities required for APT/dpkg, use the standard
  administrative `PATH`, keep `no_new_privileges`, and remain inside gVisor.

# Work Guidance

- Prefer derivable state and one direct per-user path over registries, aliases,
  migrations, background reconciliation, or per-file persistence exceptions.
- Keep temporary runtime files temporary and durable sandbox files beneath the
  one `dev-sandbox` root.

# Verification

- `go test ./kernel/development ./kernel/packages ./kernel/database/...`
  compiles the same ordinary sources as the installer. Transaction regressions
  live in their owning packages, including
  `packages/activation_transaction_test.go`.
- Build with `./build.sh`.
  `THE8020_DEVELOPMENT_E2E=1 go test
  ./kernel/development -run '^TestNative' -count=1 -timeout=15m -v`
  exercises the common `.development/runtime-bin/runsc` and prepared
  development/service images. Prepare
  `.development/runtime/development/{rootfs,image.json}` and the service image
  used by `activation_schema_test.go`. Native fixtures use short disposable
  `/tmp` paths for Unix sockets. Shared/private storage must support hardlinks.
  These checks qualify rootless gVisor; full containerd, live PostgreSQL
  concurrency, arbitrary filesystem compatibility, and host-power-loss safety
  require their separate checks.
- Native tests cover private originals, live shared reads, ignored files,
  rename/link/delete semantics, retained Git objects after shared removal/GC,
  sparse conflicts, concurrent/later edits, interrupted publication, schema and
  hook recovery, package creation/removal, and retained PTYs. Test fault
  injection belongs at test transports and executables, never in shipped code.
- The native schema fixture uses one-second Worker idle and sandbox keepalive
  timeouts, runs ordinary idle-sandbox maintenance, and joins it before runtime
  teardown. Recovery candidates must not retain one sandbox each until the
  complete fixture exits.
- Run heavy native and browser fixtures in a separate environment with explicit
  CPU, memory, and runtime limits. Keep checks on shared development instances
  small and sequential; stop if responsiveness degrades.
- Unit tests cover identity collisions, user isolation, lifecycle and idle
  admission, independent shutdown, bounded diagnostics, authorized-key
  confinement, reset boundaries, mount policy, and helper guidance.
- `TestRootlessDevelopmentE2E` covers native SSH/PTY, APT/dpkg persistence,
  helper activation, source/factory reset and root identity. Set
  `THE8020_DEVELOPMENT_E2E=1`; rootful qualification requires its separate
  `THE8020_DEVELOPMENT_ROOTFUL_E2E=1` gate on an authorized host.
- `THE8020_TERMINAL_E2E=1` enables retained-terminal checks. The package-owned
  browser/SSH and short-idle fixtures qualify display and end-to-end lifetimes.
- `THE8020_GUIDANCE_E2E=1` enables `TestRootlessDevelopmentGuidance` for real
  dev-skills publication, merged discovery, mount protection, private skills,
  and system-URL access. Unit guidance checks use a bootstrap double.
- The sibling UUI harness stages disposable nodes and packages. From `uui/`,
  with Deno on PATH and a prepared runtime root containing service/development
  images and the current common runsc under `node/kernel/bin/`:

  ```sh
  THE8020_JOB_DEFAULT_MAXIMUM_PARALLEL_WORKERS=8 \
  deno run -A --import-map=deno.local.json browser_e2e.ts \
    --source-root=/workspace/8020/kernel \
    --package-workspace=/workspace/8020 \
    --runtime-root=/workspace/8020/kernel/.development/named-terminal-test \
    --kernel=/workspace/8020/kernel/.development/bin/kernel \
    --admin=/workspace/8020/kernel/.development/bin/admin \
    --browser=/usr/bin/chromium \
    --fixture=../dev-core/programs/development-test/activation_processes.ts
  ```

  This fixture checks CLI/UUI process continuity and the legacy-private-work
  guard. Use `activation_browser.ts` for conflict editing and
  `activation_concurrency.ts` for independent publication and overlap rejection.
  Set the eight-Worker capacity at process startup for nested shell/activation
  and hook jobs. Repeat the process fixture with relocated binaries to verify
  packaging; keep `kernel`, `admin`, and `logd` together.

# Child DOX Index

- [testdata/AGENTS.md](testdata/AGENTS.md): native syscall client used only by
  workspace integration tests.
