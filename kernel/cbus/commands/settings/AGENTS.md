# Purpose

- Expose node-local settings as `kernel.config.*` recovery commands.

# Ownership

- Own thin list/get/set/unset handlers and their authoritative TOML definitions.
- Do not own setting conversion, validation, persistence, or owner-specific runtime changes.

# Local Contracts

- Query and mutation handlers accept only settings declared with node storage
  and delegate to `settings.Manager`. `the8020/system` owns global
  `system.settings.*` package commands.
- Settings list summaries are compact by default and expose complete records,
  including storage, only through the declared `detail` view.
- Set and unset share the settings transaction implementation after the storage
  boundary is checked.

# Work Guidance

- Keep response shaping consistent and behavior in `kernel/settings`.

# Verification

- Settings unit tests and application integration cover all four commands' domain paths.

# Child DOX Index

- `list/AGENTS.md`: all-settings query.
- `get/AGENTS.md`: one-setting query.
- `set/AGENTS.md`: persisted set transaction and error mapping.
- `unset/AGENTS.md`: persisted removal transaction.
