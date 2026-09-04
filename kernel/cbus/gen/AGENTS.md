# Purpose

- Discover and validate modular commands/settings and generate all static build glue.

# Ownership

- Own recursive TOML discovery, metadata validation, handler source/symbol checks, runtime-protocol schema validation, deterministic sorting, catalogs, registry, generated runtime protocol models, generated module, and two executable entries.
- Write generated build glue under `.development/generated/`, the tracked
  TypeScript protocol model under `defaults/config/runtime/protocol/generated.ts`, and the
  identical kernel-consumed Go protocol mirror under
  `kernel/runtime/protocol/`; never generate business logic.

# Local Contracts

- Public executable contract: `go run ./kernel/cbus/gen` from the exact repository root.
- Generated Go begins with the required DO NOT EDIT header and is formatted before writing.
- `defaults/config/runtime/protocol/schema.json` and
  `defaults/config/runtime/versions.toml` must agree; generation writes the
  build-module Go envelope, tracked TypeScript envelope, and byte-identical Go
  mirror in one deterministic pass.
- Handler paths are explicit, root-relative, inside `kernel/`, and package imports derive from the declared file directory.
- Duplicate IDs/keys/routes/options, missing or invalid setting
  storage, invalid types/defaults/paths, missing files, and missing constructor
  symbols fail generation.
- Extend schemas explicitly in TOML structs, validators, generated core metadata, and tests together. Ordinary prompts are valid only on required positional parameters.

# Work Guidance

- Preserve deterministic byte output and static imports; do not use runtime discovery or a generation framework.

# Verification

- `main_test.go` covers recursion, determinism, duplicates/conflicts, setting
  storage, types, ordinary-prompt constraints, missing/outside handlers, shared
  handlers, invalid symbols, catalog completeness, runtime-protocol coverage,
  protocol-version agreement, and the lifecycle command paths used by
  `run.sh`. It also executes every development and package-repository command
  example through the shared one-shot/interactive CLI parsing path.

# Child DOX Index
