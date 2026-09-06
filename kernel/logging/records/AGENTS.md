Parent DOX: [logging DOX](../AGENTS.md).

# Purpose

- Own the bounded unified log record, text codec, and process wire contract.

# Ownership

- Keep this package independent of settings, runtime, and storage. The settings
  system owns configuration; these types carry its validated policy.

# Local Contracts

- Each disk record is one UTF-8 line: full UTC timestamp, severity,
  `source/component`, bracketed bare operational IDs and declared object,
  escaped message, optional TAB plus JSON string-valued attributes, newline. The
  fixed source is `kernel`, `deno`, or `logd`; component names stay bounded.
  Parent contexts use `parent:ctx-*` to distinguish their relationship.
- Execution records include the actual `@context.username` as `user:<username>`
  alongside their declared object. Capture this invocation-local value in jobs
  and services before forwarding. Omit it when no execution principal is known,
  including ordinary kernel and anonymous native output; never invent one. The
  username retains its canonical spelling and is an exact query filter.
- Backslash, embedded newlines, tabs, and control characters are JSON-escaped.
  Decoding restores readable stacks. The complete encoded line is at most 16
  KiB; oversized text loses its middle with an explicit omission marker.
  Formatting visits only bounded prefixes/suffixes and bounded attribute
  entries.
- Valid strings already within the escaped byte budget retain their readable
  value without serialization and parsing. Plain escaped output can reuse the
  original string. These paths still validate UTF-8 and charge control escapes.
- Wire frames use a four-byte unsigned big-endian length followed by JSON, at
  most 32 KiB. Read the length before allocation; handle partial I/O. Only
  authenticated kernel control replies allow the larger query-page frame bound
  of 256 KiB plus 8 KiB for the reply envelope.
- A private-control sandbox Binding with an empty token represents recovered raw
  FIFOs only. Socket authentication remains disabled until the lifecycle owner
  supplies the existing sandbox token. Ingress uses `log-ingress/` beside the
  socket; only the kernel creates and removes these endpoints.
- No record sequence number is stored. Segment identity and byte offsets belong
  to the file owner and its query cursors.
- Query.Position is an optional filter-independent starting boundary saved in
  execution metadata; Cursor continues an existing filter-bound query. Both have
  the same bounded opaque encoding, and a request may use only Position, Cursor
  or Tail. Status.ReadPosition supplies the cached boundary without a
  per-execution control call.
- The byte/slot queue includes batches in flight until their consumer releases
  them. Warnings/errors reserve capacity and evict waiting lower-severity
  records; surviving records preserve arrival order. Loss counters have four
  fixed severity buckets, never per-context maps.
- Queries default to 100 records, allow at most 500, and return at most 256 KiB.
  Each scan advances through at most 4 MiB of file content. A continuation may
  contain no matches while advancing the scan. Filters use capture time (`from`
  inclusive, `until` exclusive), minimum severity, source, identities, parent
  context, and the exact declared object.

# Work Guidance

- Preserve identifying metadata; reject invalid metadata instead of truncating
  an identity. Credentials never belong in a record.

# Verification

- Go tests cover round-trip escaping, middle truncation, UTF-8 fragments,
  metadata validation, encoded limits, and bounded partial-frame I/O.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
