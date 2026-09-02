# Purpose

- Own the complete Phase 1 settings model and transactional runtime changes.

# Ownership

- Own modular definitions, types, node/global storage routing, conversion,
  validation, precedence, configured/active state, persisted overrides, atomic
  writes, queries, and runtime-owner registration.
- Do not bind ports, open logs, parse admin commands, or contain owner-specific
  application logic.

# Local Contracts

- Public API includes `Definition`, `Storage`, `PersistencePaths`, `ByteSize`,
  `ValidateDefinition`, `Values`, `Prepared`, `Applier`, `Info`,
  `OperationError`, and `Manager` construction/query/mutation/registration
  methods.
- Precedence is default < environment < startup argument < persisted override.
- Every definition explicitly declares `node` or `global` storage. Node
  overrides persist in `node/kernel/settings.toml`; global overrides persist in
  shared `config/settings.toml`. Both stores contain only their declared keys as
  deterministic nested TOML and use same-directory temporary files, sync,
  rename, and restrictive permissions.
- Global mutations serialize through the shared `config/.settings.lock`, reload
  the current global file while locked, and merge only the requested key so
  stale writers cannot discard unrelated overrides from another node.
- Startup strictly rejects unknown keys and settings stored in the wrong node or
  global file. Development instances are reinitialized after settings-schema
  changes instead of being rewritten in place.
- Runtime mutation is prepare → persist → commit → publish; failure discards
  preparation and preserves configured and active state.
- Restart-required settings persist configured values without changing active
  values and report restart pending until the next kernel start.
- A global setting cannot be runtime mutable until cross-node live-application
  coordination exists; current global settings are restart required.
- `logging.max_total_size` is never below `logging.max_file_size`.
- Database maximum-idle connections never exceed maximum-open connections.
  Both are runtime-mutable node settings because every kernel owns its own
  dynamic pool; maximum open defaults to 32 and maximum idle defaults to 8.
- String definitions may declare a compiled regular-expression `pattern`; the
  same constraint validates defaults, environment/startup inputs, and persisted
  mutations before they become configured state.
- A nonzero service maximum Worker default is never below its minimum.
- Canonical service defaults are zero minimum Workers, zero/unlimited maximum
  Workers, concurrency 32, target utilization 70%, Worker keepalive two
  minutes, zero minimum sandboxes, four Workers per sandbox, and session
  keepalive ten minutes. Kernel sandbox capacity defaults to 64 Workers with no
  CPU or RAM settings. Runtime supervisor heartbeat timeout exceeds its interval.
- Byte-size definitions are positive by default; a definition with explicit
  `minimum = 0` may use `0B` as an owner-documented automatic-detection sentinel.
- `github.com/pelletier/go-toml/v2` is used only to decode persisted TOML.
- Add settings only through a definition TOML plus an actual owning applier when
  runtime mutable.
- Application configuration, including every UUI protocol/timing/program
  constant, is outside this model and must not be added or cross-validated here.

# Work Guidance

- Defaults, environment names, and validation constraints belong only in
  definition TOML/generated metadata.

# Verification

- `settings_test.go` covers definitions/conversion, mandatory storage metadata,
  byte sizes, port/enum validation, all precedence layers, node/global
  persistence/load/removal, obsolete-key rejection, stale multi-writer merging,
  wrong-store rejection, configured/active state, cross-validation,
  preparation/persistence rollback, permissions, and unknown persisted data.

# Child DOX Index

- `definitions/AGENTS.md`: declarative setting schema sources.
