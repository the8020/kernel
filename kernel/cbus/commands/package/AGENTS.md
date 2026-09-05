Parent DOX: [kernel/kernel/cbus/commands DOX](../AGENTS.md).

# Purpose

- Keep the minimal package recovery surface available without package programs
  and retain implementation adapters for `the8020/packages`.

# Ownership

- Publish only `kernel.packages.list`, `kernel.packages.inspect`,
  `kernel.packages.set`, and `kernel.packages.synchronize`.
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
- Recovery list/inspect/set manage kernel-owned desired package records. Source
  inspection lists bounded refs without cloning; version listing fetches and
  reports bounded commit history. Synchronization accepts one, several, or all
  indexed packages and reports only package ID, resolved commit, and success for
  each result; Git and service-refresh details remain internal.
- Activation invokes the shared package-scoped reindex entry point after source
  publication. Command adapters neither enumerate services nor implement their
  configuration or refresh. Publication failures remain explicit package
  results.
- Local creation writes a minimal valid manifest, initializes an independent Git
  repository and first commit, and records a source-free local package row.
- Repository initialization is explicit, never inferred from discovery, and
  creates one initial commit at the package root. Remote configuration rejects
  embedded credentials. Pull fast-forwards a clean attached branch, push
  publishes it, and checkout selects a branch or detached commit. Pull and
  checkout refresh only services affected by a changed HEAD. A package record
  may select a global secret by name; handlers never resolve or return its
  value.

# Work Guidance

- Return validation failures as package data so one invalid package does not
  hide valid siblings.

# Verification

- Generator catalog and aggregate handler tests cover the four recovery commands
  and private operation adapters; package-store tests own discovery/path safety
  and real Git synchronization/authentication. Package-command tests cover
  concise package results and publication failure reporting; package-domain
  tests own schema/hook activation history.

# Child DOX Index

- [repository/AGENTS.md](repository/AGENTS.md): independent package Git
  inspection, initialization, remote configuration, status, pull, push, and
  checkout.

- The `index/list`, `index/inspect`, `index/set`, and `synchronize` leaves own
  recovery metadata; other leaves retain private handlers only.
