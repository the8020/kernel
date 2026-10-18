Parent DOX: [host kernel scripts](../AGENTS.md).

# Purpose

- Carry the complete migration history needed for supported kernel upgrades.

# Ownership

- Each numeric `major.minor.patch/` directory owns changes introduced by that
  release. Direct `.sh` files run offline; optional `after-start/*.sh` files run
  against the new kernel through ordinary administrative/package commands.

# Local Contracts

- Select `current < directory version <= target`, order versions numerically,
  and order regular `.sh` files alphabetically within each version and phase.
  Run all offline scripts, start the target kernel, then run after-start
  scripts.
- Scripts receive `THE8020_INSTANCE_ROOT`, `THE8020_UPDATE_FROM`,
  `THE8020_UPDATE_TO`, and `THE8020_UPDATE_RELEASE`; cwd is the instance root.
  PATH starts with the instance bin directory. Offline scripts use existing host
  tools and must not depend on an intermediate running kernel.
- Published scripts are immutable and remain in every later same-major release.
  Versions needing no migration need no directory. The `0.8.7` offline migration
  refreshes development helpers into a real instance `scripts/` directory,
  including already-repaired instances, without exposing node storage. Scripts
  must safely repeat after interruption; completion is recorded only after
  successful exit.
- `0.9.1` converts installations with the bundled Caddy launcher through the
  deploy `0.9.0` migration bundle, pinned by SHA-256 for amd64/arm64. Offline
  preparation validates legacy inputs and backs up node settings before
  disabling native TLS. After startup, deploy updates its required packages and
  installs the replacement launcher. One container restart stops Caddy and
  imports its final certificate state before native listeners take its ports.
  Both members of a dev/prod pair must finish their update before this restart.
  Ordinary instances without bundled Caddy skip this deployment migration.
- Each script has a five-minute timeout and five-second termination grace. A
  failure halts that phase and retains completed steps for an explicit retry.
- Data-changing scripts back up the state they change. Package activation uses
  package-owned commands after startup; never edit its catalog manually.

# Work Guidance

- Add only migrations required by an actual released contract change. Avoid
  compatibility layers and incidental package updates.

# Verification

- Add a focused updater fixture for each migration, including repeat execution.
- The deploy owner verifies `https/update.sh`, its reader, and restart import;
  native acceptance uses released kernels and the actual legacy Caddy wrapper on
  local instances, without Docker.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
