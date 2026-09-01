# Purpose

- Own the generic kernel-side Worker registry facade shared by services and
  jobs.

# Ownership

- Validate Worker entrypoints and permission subsets, start Workers through a
  selected runtime group, aggregate/list/inspect live supervisor Worker state,
  stop/kill one Worker, invoke one exact registered function locally or through
  authenticated node forwarding, and delegate job/service operations.
- Do not select runtime groups, create sandboxes, implement workload-specific
  lifecycle policy, or execute modules outside the supervisor.

# Local Contracts

- Public API includes `New`, `Manager.Start`, `List`, `Inspect`, `Stop`,
  `StopInGroup`, `InvokeWorker`, `InvokeLocalWorker`, `RunJob`,
  `ConfigureService`, and service dispatch/proxy methods.
- Worker permissions must be a subset of the parent sandbox envelope.
  Cached-only groups accept local file entrypoints; online entrypoints still
  require an explicitly allowed import host.
- Worker lookup is reconstructed from supervisor state rather than treated as
  container process state; immediate stop terminates only the selected Worker.
- A filtered `List` resolves the exact sandbox/runtime group directly and asks
  only that supervisor. It never enumerates unrelated sandboxes merely to find
  one known group.
- Invocation verifies node, sandbox, and Worker identity, never scans unrelated
  Workers, forwards cross-node only to the exact authenticated node, and returns
  bounded structured target/function/timeout/application errors.
- Workload managers with a durable Worker-to-group association stop through
  `StopInGroup`; unrelated unavailable sandboxes must not block owned Worker
  cleanup.
- Worker startup serializes admission, enforces the existing node-wide Worker
  budget, and immediately before creation re-inspects the exact target sandbox.
  It refuses creation when that sandbox is at the kernel-wide per-sandbox
  Worker limit or its sampled CPU/RAM utilization is at/above either target.
  These checks apply across workload types and do not reject dispatch to an
  already running Worker.
- An empty newly provisioned or warm sandbox may bootstrap its first Worker
  despite the supervisor-only startup resource sample; otherwise identical new
  sandboxes could reject forever. CPU/RAM targets apply before every additional
  Worker, while hard sandbox and node Worker limits always apply.
- Node-wide and sandbox-local admission failures have distinct typed sentinels;
  service placement may spill a sandbox-local rejection into another compatible
  sandbox, while creating another local sandbox cannot evade node exhaustion.

# Work Guidance

- Keep all workload types on the same start/stop path and include stable
  execution identity in debugger names.

# Verification

- Unit tests cover permission/entrypoint rejection, service/job start, exact
  sandbox Worker/CPU/RAM admission, aggregation, direct exact-group filtering,
  inspect, global and known-group stop, local/cross-node invocation, target
  mismatch and bounds, and delegated workload operations.

# Child DOX Index
