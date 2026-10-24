Parent DOX: [commands DOX](../AGENTS.md).

# Purpose

- Expose bounded node log query, tail, and cursor polling through `kernel.logs`.

# Ownership

- Adapt command arguments to the logging owner's query. Do not read files,
  retain history, route ordinary records, or implement another search engine.

# Local Contracts

- `--text` uses the logging owner's bounded literal message/attribute filter.
- `NewHTTP` adapts the same Query for authenticated development ingress. It
  strictly decodes one query of at most 16 KiB, rejects a foreign node, pins the
  immutable host identity, and returns `{node_id,page}` with no-store headers.
  Sandbox authority is checked by development before this handler; it is never
  registered on the public listener. Reads have a 30-second deadline.

- The adjacent TOML owns all flags and result metadata. `--user` is the exact
  service/job execution username; typed runtime IDs and declared objects remain
  separate filters. Capture times accept RFC3339 and are normalized to UTC.
- Return a `page` with at most 500 records and 256 KiB; the default is 100.
  Continuation cursors survive ordinary rotation and logger restart. A poll may
  advance over scanned content without returning a match. Preserve explicit
  `expired` and `unavailable` states from the file owner.
- Tail starts from the bounded recent window and exposes `tail_limited` when
  older data lies outside it. Follow repeats bounded cursor polls and stops when
  its caller cancels. No request retains a follower or a full log collection.
- --position starts from a saved execution boundary, skipping preceding history.
  It accepts fresh filters and returns their continuation cursor; do not combine
  it with --cursor or --tail. Reference expiry is owned by the shared query API.
- This command remains readable during graceful runtime shutdown until the
  administrative socket and log writer close.
- `--node` selects the exact owning node, defaulting to the local logger's
  immutable identity. Local reads require neither runtime nor topology; remote
  reads use the existing authenticated node manager. The exported Query adapter
  is shared with the SDK operation so both use identical routing and validation.

# Work Guidance

- Keep validation shared with the logging query contract. Commands do not add
  application-specific logging policy or default execution attribution.

# Verification

- `http_test.go` covers host pinning, unavailable logging, query bounds and
  rejection of foreign nodes, file paths and extra JSON input.
- Generated catalog checks and application command-bus integration exercise the
  real separate writer, bounded reads, continuation, and invalid query limits.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
