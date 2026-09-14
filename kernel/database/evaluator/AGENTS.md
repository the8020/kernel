Parent DOX: [kernel/kernel/database DOX](../AGENTS.md).

# Purpose

- Discover activated package table modules and evaluate them through the normal
  sandboxed job runtime.
- Submit package-owned schema operations through ordinary authorized jobs; keep
  TypeScript execution out of Go and credentials out of all Workers.

# Local Contracts

- Discovery is fixed-depth at `tables/<table>.ts` and uses the centralized
  canonical table ID encoder.
- Evaluate at most 256 modules per job call without static type checking, commit
  each successful initialization batch, and reuse one compatible evaluator
  Worker when possible. Failed first initialization resumes completed
  table/commit pairs.
- Use ordinary configured job grouping and profile compatibility, without a
  dedicated evaluator group. Restricted evaluator permissions belong to its
  Worker; compatible jobs may share the same supervisor.
- Mount and read only the activated shared package tree. Private development
  overlays are never schema sources; staged activation roots temporarily replace
  only their matching package mounts.
- Explicit inspection and synchronization accept activated source only when its
  checkout is clean and exactly matches the ready commit in the shared package
  index. Candidate activation evaluates its isolated staged root instead.
- Depend only on the read-only package catalog. Database-backed desired package
  and service state is composed after initial table synchronization.
- Definition-evaluator Workers have no database access, writes, imports,
  external network, administration, or credentials. Schema-application jobs use
  ordinary SQL permissions and a separate Worker/reuse identity; they never
  share the restricted evaluator's execution.
- `RunSchema` forwards opaque operation input/results to db. Its reuse version
  comes from cached publication state without source/Git scans. Candidate
  application receives staged mounts and the candidate version; rollback uses
  active sources. Go owns confinement, observed dependencies, bounded evaluation
  scheduling, and the native activation handshake; schema policy stays in db.
- Initial and explicit full synchronization discovers all package tables.
  Ordinary deployment reevaluates only new/changed/deleted definitions and
  tables whose recorded Deno module dependency closure intersects Git's changed
  files. There is no custom TypeScript or dynamic-import parser.
- `Prepare` applies candidate schema before source visibility and `Complete`
  either records activation or restores active descriptors. Restart recovery
  evaluates the package tree actually present on disk.
- Removal candidates have empty commits. Preparation retires their recorded
  tables without discovering, mounting, or executing the removed package.
- The transaction owner passes an explicit activation ID through preparation,
  completion and recovery. The evaluator retains no single current deployment
  between calls; independent package sets may progress separately. Database
  schema/catalog mutation retains its owning lock. Ordinary evaluator tests
  cover that boundary.

# Verification

- Tests cover identity, collision rejection, batching, shared job grouping with
  restricted Worker permissions, package fingerprints, and malformed results.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
