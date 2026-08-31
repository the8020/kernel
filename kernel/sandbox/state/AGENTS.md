# Purpose

- Persist small per-runtime-group desired specifications and observed status
  atomically under `node/kernel/runtime/groups/`.

# Ownership

- Own group-directory layout, restrictive modes, atomic JSON writes, loading, synchronized state transitions, enumeration, and record deletion.

# Local Contracts

- Public API: `Store`, `New`, `SaveSpec`, `SaveStatus`, `UpdateStatus`,
  `Transition`, `TransitionIf`, `Load`, `List`, and `Delete`.
- `spec.json` stores desired immutable inputs, `state.json` stores observed status, and restrictive `secret.json` stores the internal callback/control token omitted from ordinary specification JSON. Source-of-truth distinctions remain intact.
- Writes use same-directory temporary files, sync, rename, and `0600`; directories use `0700`.

# Lifecycle

- Save validated spec before host creation, persist every observed transition,
  load during reconciliation, and delete only after runtime cleanup and terminal
  history archival succeed.

# Failure Behavior

- Corrupt or mismatched records fail reconciliation explicitly; failed writes preserve the previous complete file.

# Concurrency

- One store mutex serializes writes, read-modify-write field updates,
  conditional transitions, list, and delete; returned models are value copies.

# Dependencies

- Go JSON/filesystem standard library and `sandbox/model`.

# Non-Responsibilities

- No containerd observation, supervisor health, CNI, port state, logs, artifacts, or database behavior.

# Verification

- Unit tests cover atomic persistence, reload/list ordering, legal/illegal synchronized transitions, corruption, permissions, and deletion.

# Child DOX Index
