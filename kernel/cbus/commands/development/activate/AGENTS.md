Parent DOX: [kernel/kernel/cbus/commands/development DOX](../AGENTS.md).

# Purpose

- Expose typed development activation preview and execution.

# Ownership

- Own declarative preview/run leaves and thin handler constructors.

# Local Contracts

- Requests support selection, common and per-package messages, author identity,
  metadata and lifecycle action/attempt selection. Preview is read-only and
  includes current pending/retained attempts. Run distinguishes staging, actual
  conflicts, publication recovery and cleanup; it never pushes remotes. Status,
  Continue and data-preserving Abort delegate to the same journal owner.
- Preview accepts `file` with exactly one selected package to include that
  changed file's bounded native Git diff. Unselected file contents stay omitted.
- Preview/run inspect the selected sparse workspace through the domain owner.
  Journal recovery retains captured changes across process recreation; no
  checkpoint timer or retained-tree cleanup sweep exists.

# Work Guidance

- Keep scan, merge, commit, conflict, overlay checkpoint, and publication in
  `kernel/development`.

# Verification

- Unit and real gVisor tests cover preview, helper/external activation,
  selection, messages, valid TOML metadata, conflicts, repeated publication, and
  overlay reset.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
