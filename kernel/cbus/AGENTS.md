# Purpose

- Provide one live administrative command catalog and execution protocol for
  kernel recovery commands and package-owned TypeScript commands.

# Ownership

- Own immutable process-local registry snapshots, typed requests/responses,
  package command discovery, Unix transport, and shared CLI behavior.
- Do not persist assembled catalogs or add command-specific client code,
  package capabilities, shell transport, brokers, or gRPC.

# Local Contracts

- Protocol version 2 serves `GET /v2/cbus/catalog` and
  `POST /v2/cbus/execute` over the administrative Unix socket.
- Generated Go registers only `kernel.*` recovery/lifecycle/event commands and the
  explicitly deferred command families. Active packages contribute
  flat `cbus/commands/*.toml` descriptors and same-package programs at runtime.
  Each descriptor's required `command` is its full public name; neither its
  filename nor its owning package supplies a prefix. `kernel.*` is reserved.
- The registry atomically publishes complete immutable snapshots. Execution
  loads one snapshot and holds no registry lock while a command runs.
- The package filesystem plus active database commits are the source of truth.
  Startup, activation, removal, revision change, and `kernel.reindex` refresh
  each process's catalog; invalid packages become independent diagnostics.
- Package command argv is untouched after command lookup. Secure inputs travel
  separately. Kernel adapters parse their own typed argv.
- Internal registry dispatch is transport independent and may also be used by
  an active supervised job or service through the runtime callback.

# Work Guidance

- Keep one catalog lookup, secure-input flow, help renderer, and response
  renderer across one-shot and interactive administration.

# Verification

- Core, discovery, transport, CLI, generator, and application tests cover
  atomic swaps, naming/collisions, dynamic refresh, raw argv, secure inputs,
  live dispatch, and shutdown.

# Child DOX Index

- `core/AGENTS.md`: typed protocol and registry.
- `discovery/AGENTS.md`: active package manifests and ordinary program dispatch.
- `server/AGENTS.md`: Unix HTTP server transport.
- `client/AGENTS.md`: Unix HTTP client transport.
- `cli/AGENTS.md`: metadata-driven UI behavior.
- `gen/AGENTS.md`: recursive validation and deterministic generation.
- `commands/AGENTS.md`: command grouping and handler rules.
