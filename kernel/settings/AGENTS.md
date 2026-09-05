# Purpose

- Own the complete Phase 1 settings model and transactional runtime changes.

# Ownership

- Own modular definitions, types, node-file/global-database storage routing,
  conversion, validation, precedence, configured/active state, persisted
  overrides, atomic writes, queries, and runtime-owner registration.
- Do not bind ports, open logs, parse admin commands, or contain owner-specific
  application logic.

# Local Contracts

- Public API includes `Definition`, `Storage`, `PersistencePaths`, `GlobalStore`,
  `ByteSize`, `ValidateDefinition`, `Values`, `Prepared`, `Applier`, `Info`,
  `OperationError`, and `Manager` construction/query/mutation/registration.
- Precedence is default < environment < startup argument < persisted override.
- Every setting definition declares one external environment variable beginning
  with `THE8020_`; environment names are explicit metadata rather than derived
  from setting keys.
- Every definition explicitly declares `node` or `global` storage. Node
  overrides persist as deterministic TOML in `<instance>/kernel.toml`; global
  values persist in `the8020__system__settings` and share a transactional
  revision row. Missing global values receive their declared default once;
  later default changes never replace the stored value.
- Startup strictly rejects unknown node keys. The database store validates every
  global value against its current definition before publication.
- Runtime mutation is prepare → persist → commit → publish; failure discards
  preparation and preserves configured and active state.
- Restart-required settings persist configured values without changing active
  values and report restart pending until the next kernel start.
- Global settings refresh from the shared revision and remain restart-required
  unless their owner supplies safe cross-node live application.
- `logging.max_total_size` is never below `logging.max_file_size`.
- Database maximum-idle connections never exceed maximum-open connections.
  Both are runtime-mutable node settings because every kernel owns its own
  dynamic pool; maximum open defaults to 32 and maximum idle defaults to 8.
- String definitions may declare a compiled regular-expression `pattern`; the
  same constraint validates defaults, environment/startup inputs, and persisted
  mutations before they become configured state.
- Service application defaults and their validation belong to Deno services.
  Kernel sandbox capacity defaults to 64 Workers with no CPU or RAM settings.
  Runtime supervisor heartbeat timeout exceeds its interval.
- Byte-size definitions are positive by default; a definition with explicit
  `minimum = 0` may use `0B` as an owner-documented automatic-detection sentinel.
- `github.com/pelletier/go-toml/v2` is used only for node-local TOML.
- Add settings only through a definition TOML plus an actual owning applier when
  runtime mutable.
- Application configuration, including every UUI protocol/timing/program
  constant, is outside this model and must not be added or cross-validated here.

# Work Guidance

- Defaults, environment names, and validation constraints belong only in
  definition TOML/generated metadata.

# Verification

- `settings_test.go` and `dbstore/` tests cover definitions/conversion, storage
  metadata, validation, precedence, node persistence, database default
  initialization and revisions, configured/active state, cross-validation,
  preparation/persistence rollback, and permissions.

# Child DOX Index

- `definitions/AGENTS.md`: declarative setting schema sources.
