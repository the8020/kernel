Parent DOX: [kernel defaults](../AGENTS.md).

# Purpose

- Ship host-side kernel update and launch scripts under `node/kernel/scripts/`.

# Ownership

- `update.sh` runs `update.ts` using the installed Deno binary; `launch.sh` owns
  its kernel child and keeps the enclosing container alive during updates.
- The native `kernel.update` command only detaches the installed updater and
  returns its PID and private `node/kernel/update.log` path.
- The root `release-build.sh` packages an installed tagged kernel. Docker owns
  initial seeding; these scripts never install the bootstrap package list.

# Local Contracts

- Initial scope is explicitly configured Linux rootless instances started by
  `launch.sh`, baseline `0.8.0`. Exact numeric same-major upgrades only; equal
  versions do nothing. Major upgrades, downgrades, source builds, and
  coordinated cluster migrations require separate operator handling.
- Download the selected GitHub release archive and SHA-256 file before shutdown.
  Archives contain only `release.json`, `bin/`, `scripts/`, `runtime/`, and
  `development-scripts/`; no database, users, keys, or packages.
- `node/kernel/current` atomically selects `releases/<version>`. Existing bin,
  runtime definition/image, and helper paths link through it. Retain current and
  one previous platform payload after success; no application source snapshots.
- `node/kernel/update/state.json` records from/target versions, phase, completed
  migrations, and the last failure. One advisory update lock serializes work.
  Offline application also holds the existing kernel process lock.
- A fully staged request triggers graceful shutdown. An offline failure keeps
  the launcher alive without booting either kernel; retry the same version to
  resume. Never restore old binaries automatically after migrations begin.
- After startup, verify the target build ID and generic runtime readiness.
  Application initialization failures remain visible separately. Packages change
  only through an explicit after-start migration using package commands.
- Updates interrupt live jobs, services, sessions, and terminals. Durable user
  work, configuration, identities, database, keys, and logs remain in place.

# Work Guidance

- Keep orchestration in these scripts and use existing shutdown/status commands.
  Reuse pinned Deno and host tools; add no update daemon or package policy to
  Go.
- Publish the architecture-specific archive and `.sha256` alongside each exact
  release tag. `release-build.sh` requires clean tagged source and a matching
  installed binary; build using the ordinary installer first.

# Verification

- `deno fmt --check`, `deno lint --no-config`, and `deno check --no-config`
  cover `update.ts` and `update_test.ts`;
  `deno test --no-config -A update_test.ts` exercises the production updater
  with isolated release and command fixtures.
- Native cbus tests verify detached execution, validation, and missing launcher
  diagnostics. `docker-entrypoint_test.sh` checks one-time seeding and restart
  preservation alongside existing account/readiness behavior.
- With prepared images in `.development/runtime/`, run
  `THE8020_NATIVE_UPDATE_ROOT=/root/8020/test<N> deno test --no-config -A
  defaults/kernel-scripts/update_native_test.ts`
  from the repository. The fresh single-node fixture compiles actual kernels and
  verifies real gVisor, cbus, migration, kernel replacement, and
  launcher/private-data preservation. Bound its CPU, memory, and runtime using
  the workspace's native-test guidance.

# Child DOX Index

- [update/AGENTS.md](update/AGENTS.md): versioned offline and after-start
  migration ordering, inputs, and recovery requirements.
