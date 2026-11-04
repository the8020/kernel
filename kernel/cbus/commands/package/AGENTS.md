Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Keep the minimal package recovery surface available without package programs
  and retain implementation adapters for `the8020/packages`.

# Ownership

- Publish only `kernel.packages.activation.inspect`,
  `kernel.packages.activation.resume`, `kernel.packages.list`,
  `kernel.packages.inspect`, `kernel.packages.set`,
  `kernel.packages.synchronize`, and `kernel.packages.delete`,
  `kernel.packages.synchronize.set`, and `kernel.packages.recommended`.
- Retain remaining package/source/version/local/repository thin handlers behind
  the private runtime-operation dispatcher. `the8020/packages` owns their
  visible `packages.*` command programs.

# Local Contracts

- Package source inspection delegates to the filesystem catalog; desired and
  active package state comes from the system database.
- `package list` exposes only package identity, description, validity, and any
  validation failure. `package inspect` owns filesystem paths, complete package
  metadata, fixed-depth program metadata, and the bounded non-Git file inventory
  for one selected package.
- Built-in recovery commands remain visible when package discovery, users, or
  the ordinary service plane is degraded. Source-changing mutations fail closed
  until the schema evaluator/job runtime and database deployment coordinator are
  ready.
- `kernel.packages.activation.inspect` reads one exact native activation's
  candidate and hook records. It does not resume activation or establish that
  its owner, Worker or schema is settled; never use it alone to release a lock.
- `kernel.packages.synchronize.set` without input checks protocol/schema
  capability without mutation. Its bounded JSON input selects a fresh activation
  ID and exact source/version/baseline set; secure Git input remains separate.
  `kernel.packages.activation.resume` explicitly resumes only this native owner
  under existing source and operation locks. It cannot replace development or
  legacy journals, change membership, or guarantee exactly-once failed hooks.
- Recovery list/inspect/set manage kernel-owned desired package records. Source
  inspection lists bounded refs without cloning and may name one global secret
  for a private source, applied only as a transient authorization header.
  Version listing fetches and reports bounded commit history. Synchronization
  accepts one, several, or all indexed packages and reports only package ID,
  resolved commit, and success for each result; Git and service-refresh details
  remain internal. The optional `--git-username` selects a Basic username;
  `--git-token-stdin` supplies either its password or a token securely, without
  storing either in package state.
- Activation invokes the shared package-scoped reindex entry point after source
  publication. Command adapters neither enumerate services nor implement their
  configuration or refresh. Publication failures remain explicit package
  results.
- Local creation writes a minimal valid manifest, initializes an independent Git
  repository and first commit, and records a source-free local package row.
- Deletion requires `--confirm` and delegates to `DeletePackage`; the same
  handler serves private `package.delete` calls. It removes installed source
  through activation, preserves dirty work, and retains physical database data.
- Recommendations delegate to `InstallRecommendedPackages` using the installed
  kernel list, without arguments. Results distinguish installed, updated,
  unchanged, skipped, not-applied, and failed packages. All candidates stage
  before one activation; failure details distinguish candidates never applied.
  The package store owns version/fast-forward decisions and source protection,
  including preservation of local work and custom sources.
- Repository initialization is explicit, never inferred from discovery, and
  creates one initial commit at the package root. Remote configuration rejects
  embedded credentials. Fetch refreshes refs/tags without changing the checkout.
  Pull fast-forwards a clean attached branch, push publishes it, and commit
  checkout retains the current branch. Pull and checkout refresh only services
  affected by a changed HEAD. A package record may select a global secret by
  name; handlers never resolve or return its value.

# Work Guidance

- Return validation failures as package data so one invalid package does not
  hide valid siblings.

# Verification

- Generator catalog and aggregate handler tests cover the recovery commands and
  private operation adapters; package-store tests own discovery/path safety and
  real Git synchronization/authentication. Package-command tests cover concise
  package results and publication failure reporting; package-domain tests own
  schema/hook activation history.

# Child DOX Index

- [repository/AGENTS.md](repository/AGENTS.md): independent package Git
  inspection, initialization, remote configuration, status, fetch, pull, push,
  and checkout.

- The `index/list`, `index/inspect`, `index/set`, and `synchronize` leaves own
  recovery metadata, as do `delete` and `recommended`; other leaves retain
  private handlers only.
