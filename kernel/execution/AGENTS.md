# Purpose

- Own one generic runtime-group and Worker execution system shared by services
  and jobs.

# Ownership

- Own group selection/compatibility, warm capacity, supervisor communication,
  Worker registry, exact Worker invocation, service pools/dispatch, job
  scheduling, execution artifacts, and runtime reconciliation coordination.
- Do not own containerd/gVisor/CNI/cgroups, host-port listeners, command presentation, or program business logic.

# Local Contracts

- All workload types use the same sandbox manager, supervisor protocol, Worker bootstrap, runtime profile, permissions, mounts, resources, and debugging path.
- A runtime group has exactly one workload type; shared owners share its process/security/resource/failure boundary.
- Services own independent stateless or persistent Worker pools with hard
  execution-slot limits, and jobs normally
  own one Worker per execution behind a bounded durable FIFO admission queue.
- Newly generated runtime-group IDs are `rgp-` plus eight random lowercase
  alphanumeric characters; newly generated Worker IDs are the equivalent
  `wrk-` format.

# Work Guidance

- Express workload differences only through grouping, lifecycle, scaling, permissions, mounts, routing, and scheduling; never create separate runtime backends.

# Verification

- Unit tests cover grouping, warm accounting, supervisor protocol, exact Worker
  invocation, and Worker/service/job state and scheduling; gVisor integration
  tests cover real Deno execution and cross-workload invariants.

# Child DOX Index

- `adminrun/AGENTS.md`: bounded eval/run artifacts submitted through ordinary jobs.
- `coordinator/AGENTS.md`: generic grouping selection and sandbox construction.
- `groups/AGENTS.md`: group keys, compatibility selection, and clean warm capacity.
- `jobs/AGENTS.md`: bounded FIFO admission, synchronous/detached jobs, cancellation, and compatible Worker reuse.
- `pool/AGENTS.md`: real clean-sandbox provisioning, assignment, trimming, and asynchronous replenishment.
- `profile/AGENTS.md`: safe Worker-permission-derived runtime profiles and online dependency separation.
- `records/AGENTS.md`: restrictive atomic workload registry documents.
- `services/AGENTS.md`: durable independent service pools, scaling, and streaming routes.
- `supervisor/AGENTS.md`: authenticated, streaming kernel-to-Deno supervisor client.
- `workers/AGENTS.md`: generic Worker validation, lookup, lifecycle, and workload delegation.
