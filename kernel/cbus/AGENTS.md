# Purpose

- Provide the typed administrative contract from declarative metadata through local transport to thin handlers.

# Ownership

- Own command metadata, typed requests/responses/errors, generated discovery/catalog/registry, Unix transport, shared CLI behavior, and accepted 80|20 administration commands.
- Do not use shell-command transport, runtime plugins, brokers, gRPC, or command-specific client code.

# Local Contracts

- Protocol version 1 uses `POST /v1/cbus/execute` with JSON over a Unix socket created under a restrictive umask; `server/AGENTS.md` owns exact socket-mode verification and the contained fallback for filesystems that reject socket chmod.
- Internal registry dispatch is transport independent and is reused by both the Unix administrative transport and authorized Deno supervisor callbacks.
- Production command catalog, registry, and executable entries are generated; command TOML is authoritative and handler filenames/symbols are explicit.
- Adding a command changes TOML and its referenced handler only, then runs `install.sh`.

# Work Guidance

- Share catalog lookup, aliases, argument conversion, help, rendering, and client behavior across both admin modes.

# Verification

- Package unit tests plus `app/integration_test.go` cover generation, parsing, transport, dispatch, errors, live commands, and shutdown.

# Child DOX Index

- `core/AGENTS.md`: typed protocol and registry.
- `server/AGENTS.md`: Unix HTTP server transport.
- `client/AGENTS.md`: Unix HTTP client transport.
- `cli/AGENTS.md`: metadata-driven UI behavior.
- `gen/AGENTS.md`: recursive validation and deterministic generation.
- `commands/AGENTS.md`: command grouping and handler rules.
