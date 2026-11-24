Parent DOX: [kernel defaults](../AGENTS.md).

# Purpose

- Ship host-side kernel update and launch scripts under `node/kernel/scripts/`.

# Ownership

- `update.sh` runs `update.ts` using the installed Deno binary; `launch.sh` owns
  its kernel child and keeps the enclosing container alive during updates.
- The native `kernel.update` command only detaches the installed updater and
  returns its PID and private `node/kernel/update.log` path.
- `refresh-development-scripts.sh` owns complete, staged replacement of the
  sandbox-visible instance `scripts/` directory; the source installer and Docker
  seeding reuse it.
- Docker owns initial seeding. After installing a new kernel, the updater
  invokes `kernel.packages.recommended` against that kernel's shipped package
  list. The command resolves transport through the installed deployments
  `recommended-sources` program: test/production fetch through development Git.
  Install that program before adopting the requiring kernel; unavailable source
  selection keeps completion pending, without falling back to upstream.

# Local Contracts

- Initial scope is explicitly configured Linux rootless instances started by
  `launch.sh`, baseline `0.7.4`. Exact numeric same-major upgrades only; equal
  versions do nothing. Major upgrades, downgrades, unmanaged source checkouts,
  and coordinated cluster migrations require separate operator handling.
- Fetch the selected exact Git tag and prepare it on the instance's native
  architecture with its ordinary installer in a separate initialized staging
  instance. Preinitialization suppresses bootstrap packages. Move only bin, host
  scripts, runtime definitions/images, and development helpers into the release;
  never move staging identities, keys, users, database, or packages.
- Preparation uses at most two Go processors, a 512 MiB Go memory target, a
  30-minute deadline and at least 6 GiB free temporary disk space. The old
  kernel stays running until compilation and the candidate runtime smoke
  succeed.
- Preparation streams installer stdout/stderr to the caller's update log and
  reports elapsed time every ten seconds. Report active-kernel inspection before
  querying the control plane. Keep JSON/control-command stdout captured for
  parsing; preparation output must remain live.
- Child commands concurrently drain stderr and stdout, tee progress, and retain
  at most 4,096 characters of failure detail; successful structured stdout
  retains its separate 16 MiB limit. No shared log reconstructs errors.
- Before shutdown for `0.10.17` or newer, run the installed deployments selector
  against the target list and verify the node-packages relation through an
  ordinary Worker. A missing selector, coupled package dependency, schema or
  authorized route blocks switching. Install the complete compatible
  prerequisite set with existing native synchronization first; never update
  deployments alone or silently substitute upstream. Legacy transport capability
  can require an explicitly reviewed bridge rather than source conversion.
- The private candidate-specific prerequisite receipt admits the `0.10.17`
  offline migration. Older updaters without it stop before pointer replacement.
  The target helper's `--preflight <target>` action can check an already
  prepared candidate against the original running kernel without rewriting
  either journal.
- `node/kernel/current` atomically selects `releases/<version>`. Existing bin,
  runtime definition/image paths link through it. Development helpers are copied
  into the real instance `scripts/` directory while offline, outside protected
  node storage; version `0.8.7` refreshes helpers for older updaters, including
  instances whose helper symlink was already repaired. Retain current and one
  previous platform payload after success; no application source snapshots.
- `node/kernel/update/state.json` records from/target versions, phase, completed
  migrations, and the last failure. One advisory update lock serializes work.
  Offline application also holds the existing kernel process lock.
- A fully staged request triggers graceful shutdown. An offline failure keeps
  the launcher alive without booting either kernel; retry the same version to
  resume. Never restore old binaries automatically after migrations begin.
- After startup, verify the target build ID, generic runtime readiness, and an
  ordinary `runtime.eval` Worker returning its expected value. Bound that job to
  15 seconds; leave verification pending if real sandbox execution fails.
  Application initialization failures remain visible separately. After-start
  migrations precede `kernel.packages.recommended`; completion requires that
  command to return applied/unchanged results or explicit policy exclusions. A
  selected skip leaves the installed phase pending. Current preflight binds
  non-secret source routes to the candidate commit and manifest digest. Only
  this prepared operation may replace edited selected Git trees; recheck routes
  after startup. Older receipts without routes grant no replacement authority.
- Published `0.9.14` updater and launcher scripts already use the target
  kernel's native recommendation command; the staged-set fix needs no updater
  adapter or migration. An installed pending `0.10.1` update must finish before
  selecting a later version. The README owns the narrow, preflighted i18n
  recovery procedure for the reported partially upgraded state; do not edit the
  updater journal or replace an operator's bootstrap list.
- Wait until the application initialization attempt has finished before running
  after-start migrations. Generic Worker readiness alone is insufficient while
  native package management is still being published. A completed application
  failure remains repairable; it does not independently gate these migrations.
- The `0.9.1` bundled-Caddy conversion explicitly updates required HTTPS
  packages through native package recovery commands. Its deploy-owned migration
  preserves the running proxy until a single container restart; the replacement
  launcher then imports legacy certificates and claims the existing ports.
- Existing managed instances can repair only the helper layout, without a build,
  by running
  `bash refresh-development-scripts.sh <instance-root>
  <instance-root>/node/kernel/current/development-scripts`
  and then `kernel.restart`. Stage the complete replacement before moving the
  old entry; restore that entry if publication fails.
- Updates interrupt live jobs, services, sessions, and terminals. Durable user
  work, configuration, identities, database, keys, and logs remain in place.
- Launcher shutdown waits for its kernel child to exit. Repeated shutdown
  signals cannot interrupt that wait or allow replacement startup to race the
  old kernel's instance lock.

# Work Guidance

- Keep orchestration in these scripts and use existing shutdown/status commands.
  Reuse pinned Deno and host tools; add no update daemon or package policy to
  Go.
- Use the selected tag's existing installer; do not add an updater build system
  or require CI, prebuilt archives, or GitHub credentials for public releases.

# Verification

- `THE8020_NATIVE_PREREQUISITES_ROOT=/root/8020/test<N>` and
  `THE8020_NATIVE_FRESH_ROOT=<fresh-candidate-instance>` enable
  `deno test --no-config -A update_prerequisites_native_test.ts`. The bounded
  isolated environment must support rootless gVisor. It builds real `0.10.14`,
  installs that release's actual package tags, and verifies old-set admission,
  one complete authenticated development set, partial fetch preservation and
  same-target retry. It exercises the real first-user Docker entrypoint, direct
  replacement startup, node-aware recommendations, private-work preservation,
  ordinary Workers and login. Detached launcher/migration and
  committed-activation recovery have separate coverage.
- `deno fmt --check`, `deno lint --no-config`, and `deno check --no-config`
  cover `update.ts` and `update_test.ts`;
  `deno test --no-config -A update_test.ts` exercises the production updater
  with isolated release and command fixtures, including installer output,
  heartbeats, and recommended-package failure/retry after application startup.
  The compatibility fixture reads the published `0.8.6` updater from its Git tag
  to verify old-updater execution of the new offline migration.
- Native cbus tests verify detached execution, validation, and missing launcher
  diagnostics. `docker-entrypoint_test.sh` checks one-time seeding and restart
  preservation alongside existing account/readiness behavior, including complete
  child draining under repeated shutdown signals.
- With prepared images in `.development/runtime/`, run
  `THE8020_NATIVE_UPDATE_ROOT=/root/8020/test<N> deno test --no-config -A
  defaults/kernel-scripts/update_native_test.ts`
  from the repository. The fresh single-node fixture compiles actual kernels and
  verifies real gVisor, cbus, migration, kernel replacement, and
  launcher/private-data preservation, full application initialization, and HTTP
  health/login before and after the update, and recommended package versions.
  Bound its CPU, memory, and runtime using the workspace's native-test guidance.
- The native check compiles the actual released `0.7.4` kernel, stages its
  ordinary runtime sources, and prepares the target through the real installer.
  Only the GitHub source transport is redirected to a local tagged repository.
  It bootstraps the unmanaged container layout through `docker/update.sh`,
  verifies the waiting parent survives and the original child PID becomes the
  launcher, executes an ordinary `runtime.eval` Worker through the installed
  image symlinks, verifies HTTP health/login before and after, and checks
  recommended package versions, unchanged configuration, and private work. Set
  the existing `THE8020_BOOTSTRAP_SOURCE_ROOT` to the sibling package workspace
  when testing from an isolated kernel worktree; the fixture clones compatible
  `0.7` tags.

# Child DOX Index

- [update/AGENTS.md](update/AGENTS.md): versioned offline and after-start
  migration ordering, inputs, and recovery requirements.
