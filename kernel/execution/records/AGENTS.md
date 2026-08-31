# Purpose

- Provide small restrictive atomic JSON records for workload lifecycle
  registries under `node/kernel/runtime/`.

# Ownership

- Save, load, list, delete, and quarantine one JSON document per validated
  workload ID with deterministic ordering and strict decoding.
- Do not define record schemas, implement workload behavior, store large data/logs, or replace sandbox state.

# Local Contracts

- Public API: `New`, `Store.Save`, `Load`, `IDs`, `Delete`, and `Quarantine`.
- IDs are single safe path components; directories are mode 0700 and files mode 0600; writes use temporary-file sync and rename.
- `Quarantine` atomically moves one rejected document under the registry's
  mode-0700 `quarantine/` directory, preserving its bytes while removing it
  from live `IDs` enumeration.

# Work Guidance

- Workload packages own schema and validation before/after persistence.

# Verification

- Unit tests cover round trip, ordering, replacement, deletion, quarantine,
  restrictive modes, strict decode, and path rejection.

# Child DOX Index
