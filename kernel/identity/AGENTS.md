Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Define generation and validation of opaque operational identifiers.

# Ownership

- Own the three lowercase letters, dash, ten uniformly random lowercase
  alphanumeric characters format. `New(prefix)` and `Is(value, prefix)` are the
  only Go implementation of that format.
- `NewToken()` generates separate 256-bit credentials; it never uses the
  operational ID encoding.
- Registration and collision rejection remain with the resource owner. An ID is
  never a bearer credential or proof of authority.

# Local Contracts

- `nod` identifies a persistent node. The instance owner creates it once and
  stores it in `kernel.toml`; topology registration detects another node
  claiming an existing ID.
- `sbx` identifies an actual sandbox. Sandbox lifecycle reserves it against live
  and retained state. Development lifecycle stores it separately from username
  ownership, retaining it through restart and activation but replacing it on
  factory reset. Console owner resolution rejects conflicting claims across
  providers. Sharing labels do not create resources or IDs.
- `wrk` identifies one Worker lifetime within its node and sandbox. The Worker
  registry rejects conflicting definitions for the same ID.
- `ctx` identifies one invocation, created by kernel job submission, service
  request ingress, or exact Worker control. Its scope is the node and its
  lifetime includes asynchronous continuations and full streams. Active
  registration rejects collisions; parent context preserves nesting across
  nodes. Reused Workers receive fresh contexts.
- `job` identifies an actual kernel job run from admission through completion,
  independent of the declared job/program name and reused Worker. Job admission
  owns creation and rejects active collisions; package history may retain it.
- `uis` identifies a UUI session. UUI creates it and initial database insertion
  rejects primary-key collisions. Resumes retain the ID through the session's
  lifetime.
- `mdl` identifies one retained UUI Model wrapper within a session. Model
  construction owns creation; screen registration rejects collisions among
  pending Models without replacing the existing screen.
- Jobs creates `sch` for a schedule, `jhr` for a durable queued/history row, and
  `occ` for one manual submission shared by target rows. Initial database
  inserts reject collisions; their uniqueness scope and lifetime is the shared
  Jobs database. A history row can outlive its actual `job` execution. Scheduled
  occurrence and cursor identities are structured cross-node coordination keys,
  not opaque IDs; their revision/time and schedule/node components remain.
- `srv` identifies one existing node-local service allocation and its Worker
  pool, including temporary validation allocations. Web-service placement owns
  creation, while the service registry rejects foreign allocation collisions.
  Declared service, release, generation, and placement index are explicit
  fields. Recovery retains the instance ID through Worker/sandbox restoration;
  removing the stopped allocation ends its lifetime. It is not one ID per HTTP
  request, Worker, declared service, or physical sandbox.
- `pex` identifies one persistent service binding from initial dispatch through
  keepalive/expiry/completion. Routing creates it; the supervisor registers the
  exact Worker and principal within the live service pool in its sandbox.
  Initial collisions are rejected even under the same principal. Follow-ups
  require the explicit existing-binding flag and exact Worker; they never create
  bindings. Its signed route token is a separate credential.
- `act` identifies one package activation transaction, created by the activation
  coordinator and retained with activation/hook history. Initial transactional
  insertion rejects collisions across the shared database.
- `con` identifies one attached containerd console exec, registered by task exec
  within its sandbox. `wsc` identifies one supervisor-to-Worker WebSocket relay
  connection; its live registry rejects collisions and removes it on close.
- `prt` identifies a node-local port lease through its listener and persisted
  record lifetime; the ports owner registers it. `evt` identifies one emitted
  event shared by its listener invocations; the event owner creates it. `art`
  identifies an administrative execution artifact directory; exclusive directory
  creation rejects collisions. `cor` correlates one protocol exchange. `cat`
  identifies one command registry incarnation; its revision appends a monotonic
  counter.
- Canonical names, usernames, hashes, external IDs, and credentials do not use
  this generator. A random suffix is not a uniqueness guarantee.

# Work Guidance

- Keep this module independent of runtime, storage, settings, and logging.
- Preserve entropy errors; do not replace them with a constant identifier.

# Verification

- Tests cover complete encoding, invalid prefixes, type separation, length,
  character bounds, and generation. Resource tests own collision handling.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
