# Purpose

- Expose typed development activation preview and execution.

# Ownership

- Own declarative preview/run leaves and thin handler constructors.

# Local Contracts

- Requests support selection, common and per-package messages, author identity,
  and metadata. Preview is read-only; run returns package-level committed,
  not-committed, conflicted, or failed results and never pushes remotes.
- Preview/run are the only operations that scan workspace package content; both
  use the domain's temporary Git-index path.

# Work Guidance

- Keep scan, merge, commit, conflict, and private-source synchronization in
  `kernel/development`.

# Verification

- Unit and real gVisor tests cover preview, helper/external activation,
  selection, messages, conflicts, and in-place durable source synchronization.

# Child DOX Index
