# Purpose

- Define the one small versioned JSON control protocol shared by the Go kernel, Deno supervisor, and Worker bootstrap.

# Ownership

- Own `schema.json`, message names, common envelope fields, protocol
  compatibility, and the tracked generator output `generated.ts`.
- Do not own payload bytes, transport sockets, runtime state machines, or the
  generated Go mirrors.

# Local Contracts

- Every message contains protocol version, message type, runtime-group ID, and a correlation ID where applicable.
- Unknown versions and message types are rejected clearly.
- Large request/response bodies use streams and are never encoded into control JSON.
- Generation writes build-only Go models under `.development/generated/`, the
  tracked TypeScript model at `generated.ts`, and a byte-identical Go consumer
  mirror under `kernel/runtime/protocol/`; generated output must not be edited.

# Lifecycle

- Schema changes precede generated-model and consumer changes and increment the protocol version when incompatible.

# Failure Behavior

- Invalid schema or generation fails installation before binaries or images are built.

# Concurrency

- Envelopes are immutable values and correlation IDs disambiguate concurrent operations.

# Public API

- Authoritative source: `schema.json`; generated APIs are consumed by Go and Deno runtime modules.

# Dependencies

- JSON and the repository generator only.

# Non-Responsibilities

- No business logic, payload streaming, credential validation, command
  authorization, or transport ownership; authentication, administrative, and
  database command/result envelopes carry typed control messages only.

# Verification

- Generator tests validate deterministic output, byte-identical Go copies, complete message coverage, and rejection of malformed schema.

# Child DOX Index
