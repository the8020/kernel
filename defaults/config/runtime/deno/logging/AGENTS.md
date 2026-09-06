Parent DOX: [generic Deno runtime](../AGENTS.md).

# Purpose

- Own bounded formatting and the direct logd producer transport shared by the
  trusted Worker bootstrap and supervisor. Application code keeps console and
  the existing runtime log callback.

# Ownership

- Go records/protocol.go owns the framed logd wire contract. This private Deno
  implementation uses the same version, record fields, policy and loss counters.
- Capture time and invocation-local user/context before MessagePort transfer;
  the supervisor stamps fixed node, sandbox, Worker and declared-object fields.

# Local Contracts

- Use recognizable IDs and canonical user:<username>; omit unavailable context.
  User attribution comes from the same AsyncLocalStorage scope as @context,
  including overlapping requests and reused jobs. Raw anonymous bytes never
  receive an invented user, Worker or invocation.
- Format before serialization with bounded endpoint selection, four nested
  object levels, eight members/arguments, explicit middle omission and circular
  markers. Preserve Error name/message/stack/cause; never invoke application
  toJSON, custom inspection or property getters.
- Deno/V8's native lazy Error.stack getter is the diagnostic exception. Read it
  once, then bound its text; omit application-defined stack getters/formatters.
  VM materialization of the input Error's native stack belongs to that input,
  while the logging formatter retains only its bounded result.
- Secure values are read only from the active execution. Redact against the
  original string even where truncation intersects a secret, without copying the
  unbounded input. Credential-named object fields are redacted as well.
- Record formatting budgets 12 KiB for message text and under 2 KiB for
  attributes, accounting for UTF-8 and control escapes. The Go owner enforces
  the final 16 KiB encoded line and 32 KiB wire-frame bounds.
- Worker MessagePorts charge transferred bytes plus envelope overhead until the
  supervisor admits/drops a record and returns credit. Each Worker permits at
  most 64 KiB and 64 unprocessed records, reserving 16 KiB and 16 slots for
  warnings/errors. Four loss counters replace per-context accounting. Policy
  pushes permit one outstanding message and coalesce to the latest value.
- Formatting/port failures count as dropped prints and never escape console
  calls. Failed synchronous sends consume no MessagePort credit; loss counters
  remain pending for the next successful send, including a failed loss report.
- Each supervisor owns one persistent authenticated Unix connection, a 128 KiB
  queue including in-flight writes and envelope overhead, 512 slots, a 32 KiB
  high-severity reserve, and one reusable 64 KiB write scratch. There is no
  additional batch timer before the writer. Policy pushes filter before capture
  and again before forwarding; boot/reconnect retains only that bounded queue.
- Socket writes/handshake have a 250 ms close deadline, reconnect waits 100 ms,
  and shutdown drains for at most 500 ms. Deno's Unix connect is not
  cancellable: keep exactly one attempt pending and close a late result after
  shutdown. Partial failed writes are uncertain losses and are never replayed.
  Report cumulative drops at most once a second and on close, without recursive
  prints.

# Work Guidance

- Keep policy generic and one source of truth in kernel settings. No request
  measurement policies, log history, per-context counters or ordinary log RPC.

# Verification

- Logging unit tests cover wire bounds, formatting, secret redaction, queue
  credit and producer recovery. Managed sandbox verification remains owned by
  the runtime integration paths.

# Child DOX Index

No child DOX documents. This document owns this directory.
