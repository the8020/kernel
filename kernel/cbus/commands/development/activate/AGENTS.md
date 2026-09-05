Parent DOX: [kernel/kernel/cbus/commands/development DOX](../AGENTS.md).

# Purpose

- Expose typed development activation preview and execution.

# Ownership

- Own declarative preview/run leaves and thin handler constructors.

# Local Contracts

- Requests support selection, common and per-package messages, author identity,
  and metadata. Preview is read-only; run returns package-level committed,
  not-committed, conflicted, or failed results and never pushes remotes.
- Preview/run scan the live sandbox package overlay through the domain's
  temporary Git-index path. Lifecycle checkpoints may invoke the same domain
  scanner to persist deltas across process recreation.

# Work Guidance

- Keep scan, merge, commit, conflict, overlay checkpoint, and publication in
  `kernel/development`.

# Verification

- Unit and real gVisor tests cover preview, helper/external activation,
  selection, messages, valid TOML metadata, conflicts, repeated publication, and
  overlay reset.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
