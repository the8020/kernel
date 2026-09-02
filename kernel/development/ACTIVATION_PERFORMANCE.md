# Activation performance

This note records the performance decisions behind development activation. It
is a baseline, not a release threshold: repeat representative measurements when
the package mount, Git scan, or checkpoint format changes.

## Measurement environment

- Date: 2026-09-02
- gVisor: `runsc release-20260817.0`, rootless systrap
- Host: LinuxKit 6.10, arm64, 6 CPUs
- Git: 2.39.5
- Repositories and sandbox roots: disposable temporary directories
- Cold means no disposable activation index; warm reuses the exact index from
  the preceding scan. Host page cache may be warm in both cases.
- Total timings include one runsc exec plus `read-tree`, `add`, cheap change
  detection, detailed diff, and binary patch generation. Individual stage
  timings identify which work is relevant to Preview versus capture.

Rootful runsc could not be measured in the nested development environment: its
gofer creation failed with `operation not permitted`. Docker/Podman and KVM were
not available. These are environmental gaps, not evidence about their relative
performance.

## Repository-shape matrix

All durations are representative wall-clock observations from repeated or
confirming runs on the environment above.

| Shape | Gofer cold | Gofer warm | Directfs cold | Directfs warm |
|---|---:|---:|---:|---:|
| 100 tracked files, one edit, 256 B/file | 98 ms | 38 ms | 64 ms | 38 ms |
| 1,000 tracked files, one edit, 256 B/file | 551 ms | 92 ms | 145 ms | 51 ms |
| 10,000 tracked files, one edit, 256 B/file | 2.74 s | 518 ms | 416 ms | 135 ms |
| 1,000 tracked files, all edited, 1 KiB/file | 953 ms | 664 ms | 550 ms | 428 ms |
| 10 binary files, all edited, 2 MiB/file | 1.19 s | 751 ms | 1.20 s | 771 ms |
| 1,000 tracked plus 10,000 ignored files | 576 ms | 90 ms | 103-166 ms | 48 ms |

Directfs is the material general improvement. For 10,000 tracked files it
reduced cold scan/hash time from about 2.68 seconds to 0.36 seconds. It remains
inside gVisor's filesystem implementation: the gofer owns the container
filesystem and donates only configured mount descriptors to the sentry. The
upstream filesystem guide describes directfs as the default balance between
reasonable security and avoiding gofer RPC round trips:
<https://gvisor.dev/docs/user_guide/filesystem/#directfs>.

The private package overlay was exercised with directfs enabled: edits and new
files did not reach the shared lower, untouched lower-file updates remained
visible, and recreating the sandbox discarded its live filestore. The complete
rootless development E2E also passed with directfs.

## Ignore behavior

The former forced add (`git add -A -f`) treated ignored build output as source.
With 10,000 ignored files it took about 4.0 seconds cold: 3.1 seconds in `add`
and another 0.85 seconds producing detailed and patch output. Respecting the
repository's normal Git ignore rules took about 0.58 seconds through the gofer
and 0.10 seconds with directfs, while still capturing modifications and
deletions of paths already tracked by the base commit.

Disposable indexes explicitly evict previously staged additions that become
ignored after a later `.gitignore` edit. Without that step, a warm index would
incorrectly turn an old untracked artifact into a tracked activation change.

## Temporary object database experiment

Keeping newly written Git objects beside the disposable index in `/tmp` was
compared with the ordinary repository object database:

- 1,000 edited small files: approximately 5-7% lower total capture time. The
  `add` phase improved, but detailed diff and patch generation dominated much of
  the result.
- 20 MiB across ten edited binary files: no repeatable total improvement.
- The ordinary one-file edit receives essentially no benefit because it writes
  at most one new blob.

The experiment was not retained. It adds alternate-object-directory state,
cleanup, validation, and sandbox-lifetime object growth for little benefit on
the normal path.

## Other isolated costs

- Detailed line statistics are unnecessary during activation and lifecycle
  checkpointing. For 1,000 edited text files they cost about 0.24-0.28 seconds
  through the gofer and 0.11-0.12 seconds with directfs. A quiet
  changed/not-changed comparison costs about 3-5 ms. Capture therefore uses the
  quiet comparison and reserves raw/numstat output for Preview.
- Binary patch generation dominated the 20 MiB case at about 0.70-0.73 seconds
  and did not improve with directfs. This is Git content/delta work rather than
  mount metadata latency. Large generated binaries should normally be ignored;
  genuinely versioned binaries retain this inherent cost.
- One repository with 1,000 files and ten edits took 518 ms cold / 63 ms warm
  through the gofer and 124 ms / 40 ms with directfs. Ten repositories with the
  same aggregate file and edit counts took 608 ms / 182 ms and 189 ms / 98 ms
  respectively. The remaining difference is per-repository Git process and
  index overhead; batching already removes per-repository runsc execs.
- A 1,000-file cold scan took about 585 ms on the private gofer-backed overlay,
  826 ms on an ordinary shared bind mount, 58 ms in guest tmpfs, and 10 ms
  natively on the host. Copying repositories into guest tmpfs would merely move
  the full copy and persistence cost to lifecycle boundaries, so it was not
  adopted. A shared bind mount is slower because it requires external-change
  coherence and would also discard the private-publication boundary.
- A bare runsc exec was previously measured at roughly 28 ms. Removing more
  execs can improve many-repository workloads modestly, but cannot address the
  dominant cold file walk or large-patch work.

## Current decisions

- Respect repository Git ignore rules; do not force ignored artifacts into
  preview, checkpoints, or activation.
- Use directfs with the existing gVisor-private package overlay.
- Reuse disposable indexes and their exact captured state, but keep Git objects
  in the repository object database.
- Compute detailed raw/numstat output only for Preview; capture detects changed
  packages cheaply and then exports their binary patches.
- Do not add filesystem watchers, copied host indexes with weakened stat checks,
  background prewarming, or host-visible private worktrees without a measured
  production need.
