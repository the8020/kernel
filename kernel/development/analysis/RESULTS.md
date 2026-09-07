# Development workflow experiment results

Recorded 2026-09-06 against kernel source `ce6db1b`, with the local PTY fix,
test-fixture corrections, and opt-in experiments described below. No source
package was activated, and no developer sandbox was used. This records an
analysis, not completion of the proposed production redesign.

## Environment and reproduction

- Linux `6.10.14-linuxkit`, arm64, 6 CPUs; host Git `2.39.5`.
- Repository-local Go `1.26.5`; pinned runsc `release-20260817.0`.
- Real sandbox tests used rootless systrap and the installed development image.
- tmux `3.5a` was installed only inside a disposable test sandbox.
- Host `/dev/fuse` was unavailable; opening a temporary FUSE device node returned
  `Operation not permitted`. No host FUSE mount or custom gofer build was tested.
- Timing runs were sequential. Page caches were not flushed, and host scheduling
  was uncontrolled. These are local measurements, not service-level guarantees.

Run these from the kernel repository root after its normal portable runtime and
development-image installation:

```sh
python3 kernel/development/analysis/run.py runtime
python3 kernel/development/analysis/run.py races
python3 kernel/development/analysis/run.py fuse
WORKFLOW_PROBE_FILES=10000 python3 kernel/development/analysis/run.py fuse
python3 kernel/development/analysis/git_probe.py
```

`run.py` uses Go's temporary source-overlay facility to compile the three
`workflowanalysis` fixtures into the development package. It does not replace
production files. Each experiment owns temporary repositories, runtime records,
system roots, and sandbox cleanup. The runtime test installs tmux and requires
outbound package access; the Git, race, and FUSE probes can run offline after the
toolchain and development image are installed.

## Current runtime behavior

Raw output: [runtime-results.txt](runtime-results.txt).

| Observation | Recorded result |
|---|---|
| Private edits and live lower reads | Shared `same.txt` stayed `base`; B read its private `B` while immediately seeing the host update to an untouched file. |
| Sequential A/B activation | A succeeded in 315 ms, B in 340 ms. Final same-line content was B. A's non-overlapping first-line change was also erased by B. Both activations reported success. |
| Sandbox lifetime | A's `/tmp` generation marker disappeared after activation. The persisted sandbox ID stayed the same. |
| Genuine merge conflict | Advancing shared HEAD after capture produced `conflicted` and `same.txt`. The host merge file had markers; cleanup deleted it while B retained unannotated private text. |
| Canonical helper conflict | Returned exit 3, `success:false`, `status:conflicted`, and `conflicts:[same.txt]`. Its process generation survived. The fixture matches the production command's existing structured-failure contract. |
| Abrupt runtime loss | An uncheckpointed new source file and an ignored artifact were both lost after killing/deleting runtime state and starting again. |
| Canonical helper success | Returned committed/pending-reset JSON, then the enclosing command exited 137 after about 530 ms. Its delayed post-activation marker was never written. |
| Direct console close after PTY fix | The waiting process died; before the fix, an idle read retained the PTY and left it unreachable but alive. |
| Two named tmux sessions | Shell PIDs 126 and 128 survived 20 actual authenticated console WebSocket closes/reconnections and switches, with commands and screen contents verified. No abandoned tmux clients remained. |
| Logout boundary | An authentication fixture rejected reattachment while logged out and allowed later reattachment; the tmux sessions survived. This was not a browser or real users-package logout test. |

The conflict was deliberately introduced between capture and preparation because
ordinary sequential activation currently chooses the new shared HEAD as its base
and silently overwrites instead. The runtime test exercises the real helper,
command-bus gateway, development manager, runsc, and console broker. Its command
registration and authentication are fixtures, not a booted full platform.

## Capture and lock ownership

Raw output: [race-results.txt](race-results.txt).

The deterministic fake driver wrote a file at entry to `Pause`, after activation
had captured its patch. Activation committed successfully, but neither the
published repository nor the restarted private view retained that late file.
This isolates the capture-before-pause race without relying on random timing.

While a schema hook deliberately waited for 200 ms, an unrelated reader of the
shared repository mutex could not proceed. It resumed after 221.557 ms when the
hook and activation finished. This demonstrates the broad lock's contention;
it does not prove an unavoidable deadlock or measure real schema-hook latency.

## Native Git correctness and cost

Raw samples and environment: [git-results.json](git-results.json).

All 17 cases passed: disjoint-line merge, same-line conflict, identical edits,
add/add, delete/modify, rename/edit, binary conflict, executable-mode/content
merge, symlink conflict, whitespace/newline paths, later first touch, mixed
per-path first-touch bases, mixed bases with shared renames, resolution followed
by another shared advance, stale compare-and-swap publication, plain-file
transfer, and transferred base objects preserving a rename.

Two intentionally incorrect base strategies were also rejected by assertions:
one old package HEAD falsely conflicts after a later first touch; a synthetic
base seeded from today's tree loses shared rename ancestry. Retaining the
original package tree and substituting each dirty path's observed original
passed both scenarios.

Each small-file benchmark has five trials and one edited 171-byte file. Git's
`merge-tree --write-tree -z` performs the merge; synthetic commits supply a
common parent on Git 2.39. The private-index candidate includes Git process
startup, index initialization, hashing, and tree/commit creation. The mixed-base
candidate includes a second index and synthetic base/current commits. The
two-worktree comparison includes both checkouts, patch application, a commit,
and cherry-pick; cleanup is measured separately. The merged trees must agree.

| Tracked files | Two-worktree preparation | Cleanup | Common-base candidate | Per-path-base candidate | Merge only |
|---:|---:|---:|---:|---:|---:|
| 100 | 12.0 ms | 1.7 ms | 3.2 ms | 6.6 ms | 0.6 ms |
| 1,000 | 69.1 ms | 10.0 ms | 4.6 ms | 10.0 ms | 0.7 ms |
| 10,000 | 391.9 ms | 78.7 ms | 12.3 ms | 23.1 ms | 0.7 ms |

The separate binary case changes ten distinct 2 MiB files, introducing fresh
blobs on each of three trials. Common-base candidate plus merge took 372 ms
median. The entire Git harness recorded 77,080 KiB maximum child-process RSS,
2.219 s total child user CPU, and 2.631 s total child system CPU. The RSS is a
child high-water mark across the harness, not total concurrent application
memory or filesystem-server memory.

These numbers exclude workspace snapshotting, filesystem generation bookkeeping,
conflict installation, schema validation, shared-source publication, and durable
commit acknowledgement. Index work still scales with tracked files. If schema
hooks require a materialized checkout, that additional full-tree cost must be
measured; the Git microbenchmark does not remove it.

The plain-file transfer test removes its original sandbox directory and merges
using saved originals in a new unrelated repository. That proves text recovery,
not arbitrary rename recovery. The second transfer includes a native base-only
Git bundle, removes the original repository too, and merges a shared rename.
That fixture's recorded bundle is 428 bytes; real export size depends on the baseline
objects needed, potentially including unchanged files.

## External FUSE prototype

Raw output: [fuse-1000-results.txt](fuse-1000-results.txt) and
[fuse-10000-results.txt](fuse-10000-results.txt).

The minimal Go FUSE server uses gVisor's external socket transport, without a
host FUSE mount. First write saves original and private bytes as regular host
files. Subsequent reads preserve the private version while untouched paths read
live lower changes. Removing a published private entry exposes the shared
version while the sandbox stays alive.

An ordinary ELF binary ran through the current package mount (exit 0) and failed
through external FUSE (`Permission denied`, exit 126). A Git index located on
external FUSE failed mmap (`Function not implemented`, exit 128). The scan
comparison therefore puts mutable Git indexes in normal sandbox `/tmp` storage
and measures source traversal, not the already-failed index operation.

| Files | Mount | First scan | Subsequent scans | Warm median |
|---:|---|---:|---|---:|
| 1,000 | Current private overlay | 122.703 ms | 95.514 / 93.361 / 118.060 ms | 95.514 ms |
| 1,000 | External FUSE prototype | 1,709.243 ms | 335.637 / 332.222 / 329.860 ms | 332.222 ms |
| 10,000 | Current private overlay | 544.908 ms | 146.704 / 146.232 / 148.174 ms | 146.704 ms |
| 10,000 | External FUSE prototype | 15,121.828 ms | 3,387.727 / 3,587.070 / 3,644.456 ms | 3,587.070 ms |

The server is serial and deliberately minimal, with no data/attribute caching,
production crash journal, symlink/hardlink support, or correct generation-pinned
open-handle implementation. These timings reject this prototype as a general
development backend; they are not an upper bound on optimized FUSE throughput.
Neither host FUSE through the ordinary gofer nor the official custom gofer
extension has been implemented or benchmarked here.

## Supporting production fix and verification

The only production implementation change makes SCM_RIGHTS-imported console
descriptors nonblocking before `os.NewFile`. Go can then register them with its
poller and interrupt an idle read when the console closes. The regression
`TestReceivedConsoleCloseInterruptsIdleRead` failed before the change and passed
afterward. Existing console and SSH race tests also passed. Named-session
persistence itself remains a prototype.

Two fixture corrections accompany it: the SSH E2E expects the actual opaque
sandbox hostname, and activation command registration retains structured
failure results as the production handler already does.

Use the repository-local Go environment for existing checks:

```sh
export GOCACHE="$PWD/.development/cache/go-build"
export GOMODCACHE="$PWD/.development/cache/go-mod"
export GOWORK=off
.development/toolchains/go/bin/go test ./kernel/... -count=1
.development/toolchains/go/bin/go test ./kernel/sandbox/backend/runscconsole ./kernel/console ./kernel/ssh -race -count=1
THE8020_DEVELOPMENT_E2E=1 .development/toolchains/go/bin/go test ./kernel/development -run '^TestRootlessDevelopmentE2E$' -count=1 -v -timeout=10m
THE8020_DEVELOPMENT_OVERLAY_PROBE=1 .development/toolchains/go/bin/go test ./kernel/development -run '^TestRootlessDevelopmentOverlayProbe$' -count=1 -v
```

- Rootless development E2E passed in 22.448 s, including SSH, PTY resize/input,
  APT/dpkg persistence, helper activation, and reset behavior.
- The existing rootless overlay probe passed in 3.86 s.
- PTY/console/SSH race tests passed. The corrected runtime experiment passed in
  23.008 s; deterministic race and both FUSE characterization runs passed.
- The first full Go suite hit the unchanged
  `TestLoopbackManagerAllocatesDistinctPersistentControlPorts`: two allocations
  received inspector port 33257. A focused 20-run retry passed, then the full Go
  suite passed. The port-allocation issue was not changed or concealed.
- Go formatting, whitespace, and Python syntax checks passed. The DOX audit
  checked 195 documents and all 17 workspace/repository roots with no broken
  links, missing direct-child entries, parent mismatches, unreachable documents,
  or incomplete framework copies. Sandbox-parent,
  dev-core and UUI docs remain unchanged by this task where their ownership and
  implemented contracts did not change. Workspace and relevant
  kernel/development/PTY docs record the requested target, evidence, and actual
  fix separately.

Remaining production qualification includes the filesystem backend's POSIX and
crash behavior, concurrent saves during activation/conflict installation,
schema/source transaction recovery, actual browser navigation/login/logout,
rootful deployment, and control-plane restart adoption. No complete production
workflow, browser session selector, or restart-free activation is claimed.
