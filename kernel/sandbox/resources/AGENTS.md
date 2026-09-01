# Purpose

- Translate sandbox resource profiles to cgroup v2 controls and expose authoritative group metrics.

# Ownership

- Own the unified PID cgroup setting, PID/tmpfs validation, cgroup-file parsing,
  OOM/pressure event exposure, and group-level resource observations.

# Local Contracts

- Public API: `UnifiedSettings` and `ReadMetrics`.
- Limits cover PID maximum; tmpfs bounds are passed to OCI mount construction.
  CPU and memory have no cgroup limits.
- Metrics come from cgroup v2 files and represent the complete gVisor/Deno runtime group, never an individual Worker.

# Lifecycle

- Settings are generated before container creation; metrics are sampled while the task exists and once during failure/termination where possible.

# Failure Behavior

- Missing or malformed mandatory cgroup files return contextual errors; unsupported optional `memory.peak` is reported as unavailable rather than fabricated.

# Concurrency

- Generation and reads are side-effect-free and safe concurrently; backend lifecycle prevents reading a deleted cgroup as successful state.

# Dependencies

- Go standard library and `sandbox/model`.

# Non-Responsibilities

- No cgroup directory creation, task placement, Worker-level hard limits, or scheduling.

# Verification

- Unit tests cover exact setting generation, all metric files/events, malformed input, and absent optional peak data.

# Child DOX Index
