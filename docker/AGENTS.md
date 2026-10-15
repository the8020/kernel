Parent DOX: [kernel DOX](../AGENTS.md).

# Purpose

- Own container-specific runtime assets and startup behavior.

# Ownership

- `update.sh` runs inside an existing single-system Docker container as root,
  adopts the kernel launcher, and starts the installed updater detached.
- `rootfs/` is the complete Docker runtime payload, copied to `/` by both the
  kernel and deploy Dockerfiles. Its entrypoint starts the kernel, creates the
  first login user, and waits for the public login service.
- Dockerfiles own image assembly, release metadata, and build-cache cleanup. The
  generic installer does not produce deployment payloads.
- Runtime images include `bzip2` for in-container update preparation; the normal
  installer supplies this OS prerequisite when an older root Debian image
  lacks it.

# Local Contracts

- `THE8020_INSTANCE_ROOT` selects the initialized instance (default `/8020`).
  All runtime links, bootstrap markers, and administrative calls follow that
  root so multiple instances can reuse the same container startup script.

- Fresh Docker bootstrap grants the initial user role `**` with permission
  `"*" = "*"` using `auth.roles.create --if-missing`, `auth.roles.grant`, and
  `auth.users.assign`. No authorization policy enters Go or the supervisor. The
  private `node/docker/initial-user.pending` marker retains the target username
  through interrupted account/grant steps; completion removes it and writes
  `initial-user.done`. Retries preserve an already-created account and repeat
  idempotent role grants. Existing completed volumes are not regranted.

- The kernel Dockerfile builds its local tagged checkout without build
  arguments. The tag supplies the compatible package release line through the
  ordinary installer. It never selects or checks out another kernel version.
- The deploy Dockerfile selects the newest kernel patch for its requested
  major.minor line before using the same installer and runtime payload folders.
- Copy semantic directories rather than enumerating executables or helper files.
- The final image copies packages, scripts, and runtime assets only from the
  built instance. Supply an empty private `kernel.toml` marker and empty data
  roots; do not copy the builder's database, users, node identities, master/SSH
  keys, or other node state. First startup creates fresh IDs and keys, retained
  by the instance volume. Separate fresh containers from one image remain
  independent systems.
- Dockerfiles bundle binaries, runtime definitions/images, host updater scripts,
  and development helpers under `/usr/local/share/the8020/`. Before first
  startup, the entrypoint stages these as `node/kernel/releases/<version>` and
  links bin, host scripts, and definitions/images through
  `node/kernel/current`. Copy development helpers into a real instance
  `scripts/` directory using the shared refresh script; development mounts
  must never resolve into protected node storage. Publish current last so an interrupted seed repeats.
  Later starts preserve the selected instance release even when the factory
  image differs. Seeding never touches users, packages, database, keys, or IDs.
- Existing `0.7.4` and newer containers bootstrap entirely inside the container,
  without host access, a Docker socket, or container restart. Install the host
  scripts, preserved development helpers, and new entrypoint. An atomic
  one-time executable handoff uses existing `kernel.restart` to retain the
  original kernel PID as the launcher, keeping the waiting container parent
  alive. Restore the native executable before seeding. The entrypoint private
  `--adopt` mode seeds and execs the launcher without repeating account/login
  bootstrap. The README owns invocation; old kernels have no update command.
- Bootstrap writes foreground output, handoff/seeding, wait progress, and failures
  to the private instance `node/kernel/update.log`. Start the continuation from a
  private copy of the invoked helper so newer logging also applies when targeting
  an older supported tag. Each status probe is bounded to five seconds; the
  handoff deadline is 90 seconds. Report bootstrap acceptance separately from
  update preparation. Container adoption redirects launcher startup diagnostics
  to that log.
- `launch.sh` owns the kernel child. An intentional update leaves this launcher
  and the outer entrypoint alive while the kernel stops, migrations run, and
  the target starts; the paired instance and HTTPS proxy remain running.
- The instance bin directory includes kernel/admin/logd and one common runsc
  for services, jobs and development. Keep `.development/bin/` free of a second
  runsc; the image's factory bin payload seeds the instance installation.
- Invoke the portable smoke helper from the instance's installed runtime
  definitions. Keep all startup credentials in process memory and preserve
  existing login users. Parse `users.list` JSON with the already bundled Deno
  binary; check enabled and password flags on the same user independently of
  property order. Malformed responses stop bootstrap without creating an
  account.
- Initial-account bootstrap runs once per persistent instance volume. After
  successful creation and administrator grants, or confirming an existing login
  user without pending bootstrap, write the private
  `node/docker/initial-user.done` marker. Later starts skip all user commands
  and their Deno JSON parser, including after users are deleted or disabled.
  Failed bootstrap leaves no completion marker and retries on the next start.
  The marker belongs to the container entrypoint; image building and Go never
  create it.
- Login readiness probes immediately and waits 100 milliseconds between failed
  attempts, keeping the five-minute deadline and bounded serial requests.
- Runtime containers need the documented unconfined outer seccomp profile for
  nested rootless gVisor. Docker builds use the existing enclosing build
  sandbox.

# Work Guidance

- Keep container packaging out of Go and generic installation workflows.

# Verification

- `bash docker/update_test.sh` checks argument validation, detached managed
  invocation, credential removal, private logging, and progress/retries when a
  status probe hangs. The opt-in native check
  in `defaults/kernel-scripts/update_native_test.ts` starts actual `0.7.4`,
  runs the inside-container bootstrap and real source update, and verifies
  parent/child PID retention, native executable restoration, and unchanged
  packages, configuration, and private work.
- With Deno on PATH, `bash docker-entrypoint_test.sh` checks progress, HTTP 200
  readiness, failure diagnostics, the curl dependency, existing-user detection
  with reordered JSON properties, one-time bootstrap and restart bypass after
  user removal, retry after failed creation, and rejection of invalid user
  responses without a completion marker. It also verifies initial platform
  seeding, interrupted-seed recovery, fresh smoke on restart, retention of a
  selected newer release despite changed factory assets, and private-work
  preservation. Missing bootstrap assets fail before kernel startup.
- Build both the version-selected deploy image and the local tagged-checkout
  kernel image; exercise first-user bootstrap on a fresh instance.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
