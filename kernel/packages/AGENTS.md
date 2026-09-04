# Purpose

- Own package source inspection, database-backed package/service policy, Git
  synchronization, and the atomic package activation transaction.

# Ownership

- Discover package, service, program, command, table, and hook definitions only beneath
  validated `packages/<namespace>/<repository>/` roots.
- Own database adapters for `the8020/packages` desired/active package records,
  activation history, hook runs, and `the8020/services` declarations and
  overrides.
- Own package Git source/ref inspection, clean worktree staging/replacement,
  local repository initialization, pull/push/checkout, and transient named-secret
  authentication.
- Coordinate candidate table evaluation/synchronization, pre/post activation
  hooks, source publication, durable recovery, and targeted service refresh.
- Do not own physical SQL generation, evaluator execution, runtime Workers,
  request routing, or node-local observed runtime state.

# Local Contracts

- Package and service IDs derive from validated filesystem segments. Selected
  package inspection is bounded; collection discovery is fixed-depth and never
  recursively scans every package in a hot or periodic path.
- Service manifests are declarative defaults. Installation stores a complete
  service row; operator overrides remain unchanged across package updates unless
  a changed manifest field was not overridden.
- Canonical service policy owns type, enablement, minimum/maximum Workers,
  concurrency, target utilization, Worker/session keepalive, sandbox group,
  minimum sandboxes, and Workers per sandbox. Old instance-based or filesystem
  state has no compatibility path.
- Invalid effective manifest/default/override combinations retain the
  `ErrInvalidServicePolicy` domain classification for transport adapters.
- Every direct desired-service mutation advances one shared monotonic
  `services` revision and updates that service's latest change marker in the
  same transaction. Nodes scalar-poll the revision and load only newer service
  markers. Package definition changes remain published by the package revision
  after activation, never by an early service marker while source is staged.
- The database package record is the sole desired/active package source of truth
  after first initialization. Bootstrap TOML is only the fresh-database source
  list; ordinary boots trust database state and never rescan all definitions.
  Release-staged repositories carry their resolved requested tag in local Git
  metadata; bootstrap verifies that it names the evaluated commit, then records
  the tag as desired identity and the exact commit as active identity.
- A fresh database stages every bootstrap package and performs one batched
  evaluation and synchronization before publishing the package set. Normal
  activation evaluates only candidate package tables in one batch.
- Activation is prepare schema → pre-activate hooks → atomically switch source →
  publish package/service records → post-activate hooks → complete. Durable
  activation, package, hook, and pending-deployment rows make retry/recovery
  idempotent. PostgreSQL holds one database advisory deployment lock throughout;
  SQLite uses local transactional serialization for its single-node role.
- Removed table/column definitions become retired metadata and retained physical
  data. Only explicit confirmed trim performs destructive deletion. Incompatible
  type, nullability, key, index, or constraint changes reject activation.
- Hooks are optional `hooks/pre-activate.ts` and `hooks/post-activate.ts` default
  functions. They run as bounded ordinary job Workers with package/database
  access and receive package ID, exact candidate commit, and activation ID.
- Programs resolve only from ready active package commits. Manifests may set
  `discoverable = false` without changing execution semantics. Package command
  candidates validate their referenced same-package program before source
  publication; one invocation uses a short-lived exact source snapshot so a
  self-update cannot change its running module. Source-control metadata is not
  copied into runtime snapshots.
- Synchronization clones into a hidden same-filesystem staging directory,
  validates the exact commit, and replaces source by rename. Dirty shared
  worktrees are preserved and rejected. One failed batch package has explicit
  durable failure state.
- Activated-source consumers can require a clean installed checkout whose HEAD
  or content fingerprint exactly matches the ready database commit; schema
  evaluation uses this guard while staged activation evaluates its candidate.
- Git credentials never enter URLs, command arguments, durable Git config,
  package records, results, or logs. A selected named secret or recovery command
  secure input is used only for a host-scoped transient HTTPS authorization
  header.
- Repository pull is clean-branch fetch plus fast-forward; push publishes the
  attached branch; checkout selects one local/remote branch or hexadecimal
  commit. Changed operations use the same activation transaction.
- Kernel repository SQL remains portable across SQLite and PostgreSQL. Bind
  logical booleans as parameters or use boolean predicates; never encode them
  as SQLite-only `0`/`1` literals.

# Work Guidance

- Keep mutations package-targeted. Do not turn a candidate update into an
  installed-package, table, service, sandbox, or Worker scan.
- Keep filesystem ownership separate from database ownership: this package
  supplies candidates and commits; the database owner evaluates descriptors and
  applies physical schema.

# Verification

- Tests cover identity/path and program containment/symlink validation, exact
  invocation snapshots, bounded inspection, database package and
  service CRUD/revisions, manifest defaults/operator overrides, clean Git
  synchronization and mutation, credentials, candidate-only schema activation,
  hook ordering/idempotence, durable phase recovery, PostgreSQL lock ordering,
  retirement/trim behavior, targeted package refresh, and automatic cross-node
  desired-service propagation without full service scans.

# Child DOX Index
