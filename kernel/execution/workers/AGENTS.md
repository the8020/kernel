Parent DOX: [kernel/kernel/execution DOX](../AGENTS.md).

# Purpose

- Own the generic kernel-side Worker registry facade shared by services and
  jobs.

# Ownership

- Validate Worker entrypoints and permission subsets, start Workers through a
  selected sandbox, aggregate/list/inspect live supervisor Worker state,
  stop/kill one Worker, invoke one exact registered function locally or through
  authenticated node forwarding, and delegate job/service operations.
- Do not select sandboxes, create sandboxes, implement workload-specific
  lifecycle policy, or execute modules outside the supervisor.

# Local Contracts

- `MatchingImports` targets one known sandbox and selected Workers, sends
  changed paths in batches of at most 1,024, and deduplicates matching IDs. It
  performs no global inventory lookup and stores no imports in Go or snapshots.

- Public API includes `New`, `Manager.Start`, `List`, `Inspect`, `Stop`,
  `StopInSandbox`, `InvokeWorker`, `InvokeLocalWorker`, `RunJob`,
  `ConfigureService`, and service dispatch/proxy methods.
- Worker permissions must be a subset of the parent sandbox envelope.
  Cached-only groups accept local file entrypoints; online entrypoints still
  require an explicitly allowed import host.
- Default debugger names use the validated origin type, origin ID, and Worker
  ID, matching execution context and log attribution.
- Explicit job dependency-inspection modules may use absolute sandbox paths, but
  each path must remain beneath the parent read envelope.
- Worker lookup reads the latest cached absolute supervisor snapshot rather than
  container process state. A filtered `List` resolves only the exact cached
  sandbox and never contacts a supervisor or enumerates unrelated sandboxes.
  Explicit sandbox refresh owns live inspection.
- Invocation verifies node, sandbox, and Worker identity, never scans unrelated
  Workers, carries an optional persistent-execution target for supervisor
  binding validation, forwards cross-node only to the exact authenticated node,
  and returns bounded structured target/function/timeout/application errors.
- Local and forwarded invocation apply the shared node request validator before
  lookup or forwarding. A malformed parent is rejected; a valid parent reaches
  supervisor control so reused Workers receive distinct child contexts.
- Runtime callbacks validate the cached sandbox token at the callback boundary;
  this Worker facade performs no per-call reverse liveness validation.
- Workload managers with a durable Worker-to-sandbox association stop through
  `StopInSandbox`; unrelated unavailable sandboxes must not block owned Worker
  cleanup.
- Worker operations accept only ready, active, or draining sandbox runtimes.
  Exact access to a terminal runtime returns the shared typed
  `ErrRuntimeUnavailable`; global Worker listing skips terminal groups so one
  stopped sandbox cannot make unrelated Workers invisible.
- Worker startup takes one short process-local admission reservation, enforcing
  the hard node-wide and kernel per-sandbox Worker limits from cached absolute
  snapshots plus concurrent starts. The supervisor start occurs after releasing
  that lock. A successful start remains provisionally counted until a newer
  snapshot observes it or proves it absent. CPU and RAM observations never
  reject Worker creation; admission never rejects dispatch to an existing
  Worker.
- Worker startup injects the configured non-secret database backend so module
  imports can construct the correct SQL compiler without a kernel callback. It
  also applies `ExecutionMetadata.Valid` to the canonical Worker ID, execution
  user, and workload-compatible service/module/program origin before lookup or
  contacting the supervisor.
- Node-wide and sandbox-local admission failures have distinct typed sentinels;
  service placement may spill a sandbox-local rejection into another compatible
  sandbox, while creating another local sandbox cannot evade node exhaustion.

# Work Guidance

- Keep all workload types on the same start/stop path and include stable Worker
  identity in debugger names.

# Verification

- Unit tests cover permission/entrypoint rejection, service/job start, exact
  sandbox and node Worker-count admission under concurrent starts, provisional
  snapshot reconciliation, cache-only aggregation/filtering, inspect, global and
  known-group stop, local/cross-node invocation, target mismatch and bounds, and
  delegated workload operations.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
