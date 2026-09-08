Parent DOX: [development DOX](../AGENTS.md).

# Purpose

- Investigate development workspace isolation, publication, durability, and
  terminal lifetime with reproducible, disposable experiments.

# Ownership

- Own the workflow analysis report, experimental harnesses, and recorded
  results.
- Own the Phase 1 xterm serializer qualification probe and raw broker benchmark
  observations; completed implementation status belongs to the parent's
  [implementation checklist](../WORKFLOW_IMPLEMENTATION.md).
- [REPORT.md](REPORT.md) owns recommendations; [RESULTS.md](RESULTS.md) owns
  reproduction, measurement boundaries, and verification observations.
- `current-results.json` retains the September 8 baseline refresh, including
  native Git metadata loss and inconsistent lower-file stat/hash observations.
- Production development behavior remains owned by the parent.

# Local Contracts

- Experiments use temporary repositories and sandbox roots. Never target a
  developer's existing sandbox or publish into source-workspace repositories.
- Characterization of a defect is evidence, not an accepted behavior contract.
- Keep prototypes separate from production and distinguish measured results,
  proposed behavior, and remaining validation gates.

# Work Guidance

- Measure the pinned runtime and record repository shape, environment, command,
  sample count, and the boundaries included in each timing.

# Verification

- Run from the kernel repository root with its installed local Go toolchain,
  pinned runsc, and development image.
- `python3 kernel/development/analysis/run.py runtime` exercises real sandbox
  activation, data loss, helper results, PTY cleanup, and tmux/WebSocket
  lifetime.
- `python3 kernel/development/analysis/run.py races` characterizes activation's
  capture/pause and repository-lock boundaries with deterministic fixtures.
- `python3 kernel/development/analysis/run.py git` checks native private
  commits, checkpoint/restart, and Git inspection after untouched shared files
  change. It reports failures of the existing backend; it is not candidate
  qualification.
- `python3 kernel/development/analysis/run.py fuse` runs the minimal external
  FUSE experiment; `WORKFLOW_PROBE_FILES=10000` selects the larger tree.
- `python3 kernel/development/analysis/git_probe.py` runs disposable Git merge,
  transfer, ref-publication cases, and candidate preparation benchmarks.
- Go experiments compile through a temporary source overlay and stay outside
  normal production verification. Their successful completion characterizes
  observed defects; it does not certify a completed redesign.
- `terminal_state_probe.ts` runs with the local Deno executable and
  `--no-config --no-lock`. It compares stock xterm serialization against
  uninterrupted continuation and records `terminal-state-results.json`; unequal
  cases prove missing recovery state, not accepted product behavior.
- `terminal-broker-benchmarks.txt` records the ordinary console package's
  `BenchmarkConsoleOutput` and `BenchmarkTerminalRead` results. These exclude
  the Deno terminal engine, browser, and real runsc throughput.
- `python3 kernel/development/analysis/terminal_probe.py` runs the stock probe,
  the isolated `xterm_state.ts` continuation prototype, and actual Chromium
  handoffs through `terminal_state_browser_probe.ts`. Raw JSON files distinguish
  stock failures from headless/browser prototype results. This does not qualify
  actual UUI sessions, pixel rendering, or agent compatibility.
- `terminal_state_bench.ts` measures headless parser throughput, idle engine
  heap, and snapshot capture/encoding/restore at two geometries. Run with local
  Deno, `--no-config --no-lock --v8-flags=--expose-gc`; raw measurements are in
  `terminal-state-benchmarks.json`. These are component costs, not retained
  Worker RSS or a full terminal workflow performance gate.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
