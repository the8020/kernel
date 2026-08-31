# Purpose

- Expose explicitly requested terminal sandbox history without mixing it into live sandbox inventory.

# Ownership

- Own bounded history listing and direct history-record inspection commands.
- Do not own archival, retention cleanup, or live lifecycle commands.

# Local Contracts

- `sandbox history list` is bounded and cursor-paginated.
- `sandbox history inspect` loads one immutable record and bounded archived log tails by history ID.

# Work Guidance

- Keep history identity distinct from reusable live sandbox and runtime-group selectors.

# Verification

- Generated validation and handler tests cover both commands.

# Child DOX Index

- `list/` owns the bounded history inventory command.
- `inspect/` owns direct metadata and log inspection.
