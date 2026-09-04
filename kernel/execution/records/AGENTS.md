# Purpose

- Provide small restrictive atomic JSON records for workload lifecycle
  registries under `node/kernel/runtime/`.

# Ownership

- Save, load, list, delete, and quarantine one JSON document per validated
  workload ID with deterministic ordering and strict decoding. Preload a
  process-local byte cache and ID index once so ordinary reads and listings do
  not revisit the filesystem.
- Do not define record schemas, implement workload behavior, store large data/logs, or replace sandbox state.

# Local Contracts

- Public API: `New`, `Store.Save`, `Load`, `IDs`, `Delete`, and `Quarantine`.
- IDs are single safe path components; directories are mode 0700 and files mode
  0600; writes use temporary-file sync and rename. Mutations serialize only by
  a bounded striped per-record lock, then update the cache atomically; unrelated
  records remain concurrent.
- `Load` and `IDs` normally use the in-memory cache. A cache miss may read one
  exact record for recovery, but must not scan the registry directory.
- `Quarantine` atomically moves one rejected document under the registry's
  mode-0700 `quarantine/` directory, preserving its bytes while removing it
  from live `IDs` enumeration.

# Work Guidance

- Workload packages own schema and validation before/after persistence.

# Verification

- Unit tests cover round trip, startup preload, cached reads/listing, concurrent
  unrelated records, ordering, replacement, deletion, quarantine, restrictive
  modes, strict decode, and path rejection.

# Child DOX Index
