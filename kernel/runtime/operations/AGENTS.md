Parent DOX: [kernel/kernel/runtime DOX](../AGENTS.md).

# Purpose

- Adapt trusted runtime package calls to kernel-owned implementation primitives.

# Ownership

- Own the private operation name to implementation mapping used by
  `@the8020/kernel`.
- Do not publish CBus commands, parse package command lines, or define package
  policy.

# Local Contracts

- `service.restart` accepts exactly a service ID and `soft` or `hard` mode and
  delegates to generic lifecycle publication/reconciliation. No application
  configuration edit or package reindex is required to force fresh capacity.

- Operations call handlers or managers directly; they never recurse through the
  public command registry.
- `package.delete` shares the confirmed deletion handler with
  `kernel.packages.delete`; source/catalog lifecycle belongs to packages.
- `terminal.*` delegates physical PTYs to the shared console manager. It owns
  only the calling Worker's replaceable attachment references and pending-open
  cancellation. The trusted sandbox/Worker pair comes from callback context,
  never application input. Release cancels pending opens and detaches all roles
  without closing existing PTYs; empty owner records are removed.
- Terminal input acknowledges native consumption for bounded frame flow control;
  output reads wait on owner events. Deno labels, rendering, and client
  protocols stay outside this bridge.
- `terminal.open` delegates sandbox-scoped connect-or-create to the broker. Its
  processor descriptor must match the trusted node, sandbox, and Worker and
  contain a canonical persistent execution ID. Return either the live owner or a
  new lease with its initial sequence and display-reset flag. Failed adoption
  releases only the new lease; failed creation also closes its new PTY.
- Canonical terminal responses acknowledge bounded admission so a process that
  writes before reading cannot deadlock the display processor. Physical
  destruction is idempotent for an already absent identity.
- `terminal.close` optionally addresses an exact `nodeId` through authenticated
  node control. Local calls and recipient requests share `CloseTerminal`, which
  releases attachment bookkeeping and destroys the physical PTY independently of
  its display Worker's lifetime. It never creates a replacement terminal.
- Native display accept/write/finish operations require the exact Worker's
  canonical processor lease. Streams share the retained attachment bound and
  expose only opaque bytes and physical sequence/geometry. Control contention
  returns a typed SDK outcome; explicit takeover revokes the prior transport.
- Physical destruction returns a typed closed outcome to blocked processors and
  other attachment operations, so package owners can complete their lifecycle.
  Process EOF and Worker/attachment loss remain distinct from destruction.
- `service.route` validates exact infrastructure references and uses the
  deployment signer without creating a binding or consulting application
  metadata. The service router and supervisor enforce live ownership and the
  principal on the subsequent request.

- `logs.query` strictly decodes the shared bounded log query and uses the same
  local/exact-node adapter as `kernel.logs`. The file search remains in logd.
- Targeted service refresh is exposed through the same typed implementation as
  its CBus command; it refreshes only the selected service's relevant sandboxes,
  never the complete runtime.
- Settings operations enforce their declared global or node storage boundary.
- Cryptographic operations delegate to the kernel deployment signer. Arbitrary
  bytes use base64 on the existing JSON bridge; JWT helpers use the explicit
  platform token profile. Private keys never cross the bridge. Password/account
  and session policy belongs exclusively to Deno packages.
- `event.emit` queues local asynchronous package listeners using the caller's
  execution user. `program.run` submits an ordinary program with inherited or
  selected user, sandbox group, and timeout, returning status/result, allocated
  node/sandbox/Worker/job/context IDs, saved log position, and invocation times
  even on execution failure. It returns no log messages. Program selection
  delegates to the package catalog. Application schedule/history operations
  belong to the jobs Deno package.

# Work Guidance

- A new operation must expose a necessary kernel foundation through a thin typed
  contract. Compose application workflows in Deno; keep operation validation,
  cancellation, identity, and cleanup with the shared infrastructure owner and
  verify the full bridge path.

# Verification

- Terminal operation tests cover Worker identity isolation, native input/output,
  release during creation, live lease cleanup without process destruction,
  reattachment, and explicit close after display-Worker loss. Run with the
  node/console/callback race tests.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
