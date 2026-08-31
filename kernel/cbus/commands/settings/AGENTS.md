# Purpose

- Group settings inspection and persisted-override mutation commands.

# Ownership

- Own thin list/get/set/unset handlers and their authoritative TOML definitions.
- Do not own setting conversion, validation, persistence, or owner-specific runtime changes.

# Local Contracts

- Query handlers delegate to `settings.Manager`; detailed records expose each
  definition's declared node/global storage, and mutation handlers translate
  owned failures to stable command-bus codes.
- Settings list summaries are compact by default and expose complete records,
  including storage, only through the declared `detail` view.
- Set and unset both use the same settings transaction implementation, which
  routes the override to the store declared by the setting rather than by the
  caller.

# Work Guidance

- Keep response shaping consistent and behavior in `kernel/settings`.

# Verification

- Settings unit tests and application integration cover all four commands' domain paths.

# Child DOX Index

- `list/AGENTS.md`: all-settings query.
- `get/AGENTS.md`: one-setting query.
- `set/AGENTS.md`: persisted set transaction and error mapping.
- `unset/AGENTS.md`: persisted removal transaction.
