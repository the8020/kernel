# Purpose

- Define the transport-independent typed command contract and static registry.

# Ownership

- Own metadata types, request/response envelopes, stable error codes, handler type, argument validation/conversion, IDs, and dispatch.
- Do not own sockets, CLI tokens, settings, or domain behavior.

# Local Contracts

- Public API is the exported protocol constants/types, `NewError`, `NewRegistry`, registry methods, `NewRequestID`, and `PathString`.
- Registry validation runs for every transport request before a handler; unexpected errors become safe internal errors and are logged with request ID.
- Supported command parameter types are string, integer, and boolean; parameters may be ordered positionals or unique long options. A required positional may declare an ordinary client prompt used only when its command token is omitted. A metadata-declared secret is a required string acquired by clients through a secure prompt or its explicit standard-input flag and is never an ordinary positional or option value.
- Extend protocol metadata only when command TOML generation and both clients can consume it generically.

# Work Guidance

- Preserve stable error code strings and never expose stack traces.

# Verification

- `core_test.go` covers typed conversion, required arguments, dispatch, and unknown commands.

# Child DOX Index
