# Runtime Deno cache qualification

Owner: [runtime DOX](AGENTS.md). Measured 2026-09-07 on Linux ARM64, 6 CPUs,
Deno 2.9.4, gVisor release-20260817.0, rootless systrap.

## Decision

Service/job/module sandboxes share writable node-local `npm`, `remote`, and
`gen` directories. Ordinary imports populate them at runtime, including imports
first discovered after another sandbox starts. The remaining `DENO_DIR` stays in
each sandbox's bounded tmpfs. Development sandboxes keep their separate cache.
The [composition contract](../../../kernel/app/AGENTS.md) owns paths and
lifetime.

This reuses downloaded sources, extracted npm packages, and transpiled
JavaScript. SQLite-backed type-check, analysis, and V8 caches remain private.
npm lifecycle scripts, application bundles, and other explicit build commands
are not made shared build artifacts by this change.

Deno already uses atomic publication for
[npm extraction](https://github.com/denoland/deno/blob/v2.9.4/libs/npm_cache/tarball_extract.rs)
and
[transpiled files](https://github.com/denoland/deno/blob/v2.9.4/libs/resolver/cache/disk_cache.rs),
with
[source/version validation](https://github.com/denoland/deno/blob/v2.9.4/libs/resolver/cache/emit.rs).
Its
[SQLite caches use WAL and mmap](https://github.com/denoland/deno/blob/v2.9.4/cli/cache/cache_db.rs),
while
[gVisor file locks are local to a sandbox](https://github.com/google/gvisor/blob/release-20260817.0/pkg/sentry/fsimpl/gofer/gofer.go#L2346).
Sharing the entire writable cache across gVisor sandboxes failed below.

Concurrent first misses can still duplicate downloads. A lock around a whole
service process would prevent other services from starting for its lifetime.
There is no custom loader, preparation service, dependency scanner, or source
snapshot. Retained entries use ordinary node disk space; no automatic eviction
is added. This derived cache can be removed with the kernel and its sandboxes
stopped; the next startup recreates its directories.

## Eight-sandbox comparison

Each batch starts eight separate gVisor sandboxes simultaneously, runs
`deno run --check --no-config --no-lock --node-modules-dir=none /probe.ts`,
checks the imports' exported values, and destroys every sandbox. Each round
starts with an empty node cache and follows with eight new sandboxes. Three
rounds per safe mode; medians below include sandbox startup, checking, and
execution.

| Shared cache               | Cold batch | Following batch | Downloads in following batch |     Successful executions |
| -------------------------- | ---------: | --------------: | ---------------------------: | ------------------------: |
| None; private tmpfs        |    6.696 s |         5.831 s |                          136 |                     48/48 |
| `npm`, `remote`            |    6.699 s |         4.660 s |                            0 |                     48/48 |
| `npm`, `remote`, `gen`     |    6.825 s |         4.502 s |                            0 |                     48/48 |
| Entire writable `DENO_DIR` |   rejected |               — |                            — | 4/8; four exit 135/SIGBUS |

The shared-cache following batches use `--cached-only` and disabled networking.
For the selected mode, cold downloads numbered 111, 114, and 111 across eight
simultaneous readers; one isolated reader downloads 17 resources. This exposes
the remaining first-miss duplication. There were no partial-package failures in
the 48 executions of the selected mode.

The probe imports Zod 4.1.5, Kysely 0.29.4, smol-toml 1.8.0,
`@noble/hashes/argon2.js` 2.4.0, `@std/toml` 1.0.11, and
`@std/collections/deep-merge` 1.3.0. Bind mounts use gVisor's shared filesystem
validation; only the rootfs receives a private overlay. Cache tmpfs is 128 MiB.

A whole-cache immutable seed with private copy-on-write overlays also passed
eight offline readers and reused type-check results. It does not publish later
misses to other sandboxes, so it does not meet the runtime-growth requirement.

## Actual system startup

Baseline: kernel `f4c6fe0` / 0.4.3. Candidate: the same source plus this cache
change and rebuilt generic image. Both use the same installed 0.4 package set.
The real release Docker entrypoint runs against an extracted image in an
isolated user/mount namespace, with disposable instances and high HTTP/SSH
ports. This measures runtime startup, not Docker build time or container-daemon
startup.

The timer starts at entrypoint launch and ends at `80|20 is ready`; every run
also verifies HTTP 200 at `/the8020/uui/login/`. Fresh runs create the default
initial user in a new database. Restarts reuse that database, verify bootstrap
is skipped, and retain the candidate's populated cache. All sandboxes are
destroyed between runs. Downloads count Deno `Download` log entries, not bytes.

| Scenario                       | Three samples (seconds) |   Median | Downloads per startup |
| ------------------------------ | ----------------------- | -------: | --------------------: |
| Fresh, private cache           | 53.900, 52.462, 49.564  | 52.462 s |                   441 |
| Fresh, shared file cache       | 33.327, 33.138, 33.905  | 33.327 s |                    17 |
| Restart, private cache         | 25.808, 23.862, 24.741  | 24.741 s |                   210 |
| Restart, retained shared cache | 14.495, 14.285, 15.717  | 14.495 s |                     0 |

Median first startup improves 36.5%; normal restart improves 41.4%. Sandbox
creation, private type checking, package initialization, and service startup
still cost time. These measurements do not promise the same timings on another
host or network.

## Regression checks

`kernel/app/runtime_test.go` verifies exact shared mounts, writable image-user
permissions, persistence through profile recreation, and private SQLite storage.
The existing real rootless service/job harness checks newly discovered imports
in both directions after both sandboxes and the service Worker are running. Its
local dependency server refuses subsequent requests, and emitted-file mtimes
must remain unchanged on the second import.

From the kernel root, using the installed pinned Go toolchain and current image:

```sh
go test ./kernel/app
THE8020_RUNSC_E2E=1 \
THE8020_RUNSC_PATH=/absolute/instance/node/kernel/bin/runsc \
THE8020_RUNTIME_ROOTFS=/absolute/instance/node/kernel/runtime/images/rootless/rootfs \
go test ./kernel/sandbox/backend/rootless \
  -run '^TestRealRunscSupervisorUsesMountedKernelSocket$/concurrent_service' -count=1 -v
```

Go and generated-module tests, generic Deno checks and all 105 tests, the
rebuilt portable image smoke, and the service/job cache regression passed. The
complete opt-in rootless harness has an existing failed native-type-check
job-log assertion, reproduced with the unchanged baseline test and without
shared cache mounts. Full containerd integration was not available; its OCI unit
tests passed. Timings and real sandbox qualification here apply to rootless
ARM64.
