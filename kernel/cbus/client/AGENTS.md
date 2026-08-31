# Purpose

- Send typed command-bus requests to one local Unix socket.

# Ownership

- Own Unix dialing, HTTP request encoding, response decoding, timeouts, request defaults, and idle connection closure.
- Do not parse command lines, render results, or call services.

# Local Contracts

- Public API: `Client`, `New`, `Execute`, and `Close`.
- Requests and responses use `core` envelopes and protocol version 1.
- Response decoding preserves JSON numbers as `json.Number` so human output never changes large integers into scientific notation or loses integer precision.
- Extend transport addressing only when a future accepted administration transport exists.

# Work Guidance

- Keep command behavior generic and metadata independent.

# Verification

- Server transport and application integration tests exercise the real client.

# Child DOX Index
