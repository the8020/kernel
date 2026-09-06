Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own kernel log producers, the dedicated logd lifecycle, and logging settings.

# Ownership

- Own bounded slog formatting/transport, process supervision, trusted producer
  registration, raw descriptor lifetime, cached status, and policy publication.
  The daemon alone opens persisted log segments and owns readers.
- Preserve intentional console output through saved original descriptors. Admin,
  SSH, and interactive development consoles keep their presentation owners.

# Local Contracts

- Config supplies paths, canonical node identity, producer capacity including
  the kernel, the policy from settings, and whether this process owns standard
  descriptor capture. logd defaults to the companion executable beside kernel.
  Startup/restart is asynchronous; missing or failed logd leaves administration
  available. NodeID exposes the immutable owning node; Logger, Status, Query,
  Prepare, sandbox registration/retirement, Console, and Close delegate to that
  owner. The configured live producer budget includes the kernel and application
  sandboxes; it never grows with retained history.
- The seven logging settings have one authoritative definition in settings:
  enabled, level, split_by, split_period, max_file_size, max_total_size,
  max_age. Defaults are true, info, none, day, 128 MiB, 10 GiB, and seven days.
  Physical splitting is unified or by source; identities select filtered views.
- ReadPosition returns a cached, filter-independent file boundary with no I/O.
  Execution owners capture it before their first log and retain it with node,
  invocation IDs and time ranges. Query.Position starts a filtered view there;
  subsequent pages use Cursor. The cache can precede an invocation, so readers
  still filter overlapping traffic. References expire with their segments and
  may be absent before the first available writer snapshot.
- Kernel structured records use one persistent authenticated Unix connection.
  The kernel queue includes in-flight records within 128 KiB and 512 slots,
  reserving warning/error capacity. Reuse one 64 KiB write scratch; do not add
  another batching delay or retain per-execution history. Delivery after a
  failed socket write is uncertain: count the batch conservatively and never
  replay it as though delivery were known to have failed.
- A private inherited control socket serializes at most eight queued requests.
  Caller cancellation returns promptly; consume any response already in flight
  so subsequent requests retain their framing. File operations remain in logd.
  Cached status performs no external I/O and includes process/storage state,
  bounded loss counters, byte budgets, writer incarnation and restart count.
- Prepare opens replacement outputs before settings persistence. Commit only
  publishes the complete desired policy and wakes process control; it performs
  no filesystem/socket I/O under settings locks. Producer filters change
  immediately. policy_pending exposes writer convergence; an expired or lost
  preparation restarts logd with the persisted desired policy. Discard keeps the
  working policy; uncommitted resources expire after five seconds.
- The kernel sender waits for logd's policy push before forwarding records under
  newly enabled or widened filters. Policy publication, queue admission and
  batch selection share a short lock; formatting and socket I/O stay outside it.
- RegisterSandbox uses the existing sandbox token, creates its two private FIFOs
  before native startup, and binds the supervisor directly to logs.sock. Live
  registrations are replayed after logger restart. Keep FIFO inodes and standby
  read descriptors stable; while logd is unavailable, bounded readers drain/drop
  bytes and count them so native producers avoid SIGPIPE or an indefinite full
  pipe. Pause standby reads before handing input to logd.
- Before database/runtime startup, scan the private ingress directory once for
  canonical sandbox FIFO names and reopen standby descriptors. Bound the scan
  and registrations by configured producer capacity; raw_recovery_error exposes
  incomplete recovery. Adopted streams are readable immediately, but socket
  authentication stays disabled until the lifecycle owner restores the original
  sandbox token. Never invent replacement credentials.
- Standby sandbox readers pause while the previous logd holds the writer lock;
  the replacement kernel's own raw pipes drain independently. The new writer
  adopts the same FIFO inodes after its predecessor finishes, without waiting
  for database initialization or supervisor health.
- UnregisterSandbox follows native process stop, waits for available-tail
  drainage, then removes only that registration's unchanged FIFO inodes. The
  registry scales with live producers, not retained logs or invocation history.
- When a lowered producer limit or bounded startup scan leaves inherited FIFOs
  unadopted, confirmed native cleanup still checks and retires that owner's two
  named endpoints directly. It allocates no registration, reader goroutine, or
  history. Directory-relative no-follow opens and matching FIFO inodes preserve
  other sources, replacement endpoints, ordinary files, and symlink targets.
- Failed endpoint removal retains ownership for cleanup retry. Preparation
  rollback removes only newly created endpoints. Manager shutdown closes its
  descriptors without unlinking still-registered FIFOs; only confirmed native
  deletion authorizes unlinking them.
- Managed kernel startup replaces fd 1/2 with raw pipes inherited by logd;
  native panic output therefore survives kernel-process death. Keep original
  console descriptors for intentional output and logger emergency diagnostics.
  Close stops structured intake, restores stdout/stderr, releases write ends,
  and lets logd drain before bounded termination of only its owned child.
- logd has its own process group and no parent-death kill signal. Control EOF
  triggers its bounded drain. Writer restart retries once per second; failure
  events are rate limited and recovery is recorded. Killed-producer buffers,
  overload, prolonged storage failure and host failure can lose records,
  including errors. Dirty sync is approximately once a second.
- Usernames come from the active job/service execution context and appear as
  user:<username>. Preserve their canonical names and omit this metadata when no
  execution user is known; do not assign users to anonymous native bytes.

# Work Guidance

- Use log/slog and the dedicated writer; do not add a logging framework.
- Keep policy generic: transport, formatting, batching, writing, and retention.
  Request measurement and statistical summaries belong to their emitting
  application/runtime module, not HTTP-specific logging settings.
- Preserve identifying metadata and both ends of oversized message/stack text
  with an explicit middle-omission marker and valid encoding. Bound formatting
  itself; secure inputs and connection credentials never enter storage.

# Verification

- manager_test.go builds the real companion and exercises early boot, policy
  prepare/discard/commit, filtering, bounded outage queues/raw drainage, restart
  and FIFO continuity, cancelled queries, and native kernel stdout/stderr plus
  real Go panic drainage in a separate child process.
- Kernel-replacement tests keep a previous writer alive, verify noncompeting raw
  adoption before runtime recovery, reject socket authentication on raw-only
  bindings, restore the original credential, and preserve endpoint ownership
  through failed preparation and cleanup.
- Reduced-capacity restart tests verify visible incomplete adoption followed by
  complete owner-scoped FIFO retirement; unadopted cleanup rejects regular files
  and symlink directories/endpoints without adding live readers.
- Formatter tests cover grouped credential redaction, cyclic values, middle
  truncation and errors whose formatter panics. Store/codec tests belong to the
  child owners. Native process tests do not replace sandbox/Deno E2E checks.

# Child DOX Index

- [records/AGENTS.md](records/AGENTS.md): bounded records, compact text,
  authenticated wire types, transport queues, and query contract.
- [daemon/AGENTS.md](daemon/AGENTS.md): dedicated ingress, file owner, rotation,
  retention, storage recovery, and bounded readers.
