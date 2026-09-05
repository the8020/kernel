Parent DOX: [8020 workspace](../AGENTS.md).

Framework source:
[agent0ai/dox/AGENTS.md](https://github.com/agent0ai/dox/blob/765ae4ac02cc884eefcd41a3d0f71941721adb89/AGENTS.md).

# DOX framework

- DOX is highly performant AGENTS.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable
  docs must stay understandable from the nearest applicable AGENTS.md plus every
  parent AGENTS.md above it

## Read Before Editing

1. Read the root AGENTS.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every AGENTS.md found along each route
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path,
   read that child and continue from there
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide
   rules
7. If docs conflict, the closer doc controls local work details, but no child
   doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session
before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or
  quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child
index changes. Update child docs when parent changes alter local rules. Remove
stale or contradictory text immediately. Small edits that do not change behavior
or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root AGENTS.md is the DOX rail: project-wide instructions, global preferences,
  durable workflow rules, and the top-level Child DOX Index
- Child AGENTS.md files own domain-specific instructions and their own Child DOX
  Index
- Each parent explains what its direct children cover and what stays owned by
  the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child AGENTS.md when a folder becomes a durable boundary with its own
  purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user
  instructions; if there are no specific standards or instructions yet, leave it
  empty
- Verification must reflect an existing check; if no verification framework
  exists yet, leave it empty and update it when one exists

Default section order:

- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local
  version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for
  risks that no longer exist

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## User Preferences

When the user requests a durable behavior change, record it here or in the
relevant child AGENTS.md

- User-visible descriptions, hints, placeholders, notices, and empty-state copy
  must help the user act or understand a user-visible outcome. Never add copy
  solely to explain internal architecture, storage, persistence, sessions,
  transport, or implementation details; omit it entirely and keep those details
  in DOX or developer documentation. For example, never show
  `Value is stored per-session in the user storage.` or
  `The value is sent directly to kernel secret storage and is not shown again.`
  in the UI.
- `run.sh` and the kernel binary may run from any current directory. `run.sh`
  initializes an absent fixed-layout instance before building images. The only
  node configuration file is `<instance>/kernel.toml`; shared durable state is
  database-backed. Both validate Unix permission support and keep locks,
  sockets, logs, images, and observed runtime data under `node/`. Runtime
  instances never use the source repository's `.development/` tree as state or
  create legacy top-level `config/` or `state/` roots. The interactive wrapper
  repairs inherited terminal state before output and on exit so an interrupted
  raw-mode client cannot cascade line indentation.
- The sibling `the8020/deploy` repository owns the release Dockerfile. A
  `VERSION=<major.minor>` build resolves the newest kernel patch in that exact
  release line from GitHub and never copies local kernel or package sources. The
  selected kernel runs the ordinary installation in `/8020`, resolves each
  first-party package to its newest compatible tag, and materializes both
  rootless service and development sandbox images. Package tags must share the
  kernel major and may use the requested minor or an older one; the highest
  compatible minor and patch win. The exact selected package tag and active
  commit are retained in the database package index. Missing compatible tags
  fail installation. Docker runs require an unconfined outer seccomp profile and
  complete the pinned runsc smoke before kernel startup. The entrypoint creates
  the first enabled login user when none exists, defaulting to username `admin`
  and password `admin`; `THE8020_USERNAME` and `THE8020_PASSWORD` independently
  override those defaults. Enabled passwordless users do not suppress this
  bootstrap. Existing users are never changed. It prints `80|20 is ready` after
  first-user handling. The kernel control plane remains independent of users,
  while Docker readiness requires this initial user to exist. Bootstrap failure
  prints the last users-command error and kernel status so the underlying
  runtime failure is visible in container output.

- Interactive kernel shutdown must acknowledge `Ctrl-C` immediately and emit one
  ordinary line whenever the graceful-shutdown stage or completed-stage
  percentage changes until the process exits; unchanged status polls must never
  repeat output. Independent cleanup must overlap where its ownership
  dependencies allow.
- The administrative console exposes `kernel.restart` and `kernel.shutdown` and
  remains open after either request. It has no compatibility aliases. Restart
  performs the same graceful cleanup and then replaces the kernel process with
  the current binary, independent of whether the active `run.sh` owns that
  process.
- Uncaught UUI program exceptions must open the package-configured standard
  Program terminated short-dump program without ending the session; it must
  expose bounded exception, stack, and source context, support copying the dump,
  and allow returning directly to the package-configured Home program.
- Every kernel setting definition must declare whether its persisted override is
  node-local or global. Node overrides belong in `<instance>/kernel.toml`;
  global overrides belong in `the8020__system__settings`. Both use the same
  typed settings commands. Every externally configurable kernel-setting
  environment variable uses the `THE8020_` prefix. Application-specific
  configuration is not a kernel setting and is never injected into generic
  request metadata.
- Service manifests declare `stateless` or `session` lifecycle, positive session
  keepalive, minimum/maximum Workers, concurrency and target utilization per
  Worker, positive Worker keepalive, optional sandbox group, minimum warm
  sandboxes, and positive Workers per sandbox. Minimum and maximum Workers
  default to zero; zero minimum permits scale-to-zero and zero maximum is
  service-unlimited while kernel capacity still applies. Worker and session
  keepalive default to two and ten minutes respectively. The Deno services
  package owns declarations, defaults, overrides, and effective versions. Go
  consumes only validated package-scoped runtime specifications in its derived
  memory index; it never parses service TOML or queries application service
  tables.
- Concurrency per Worker equal to one is strict. Larger values are balancing and
  autoscaling targets with at most one temporary extra request per Worker while
  scale-up catches up; kernel reservations are short-lived routing hints and
  supervisor snapshots remain observed truth.
- HTTP, streaming, SSE, and WebSocket transports are available to both service
  types. `the8020-route` carries a deployment-signed `the8020-route+jwt` with
  exact node, sandbox, Worker, and persistent execution IDs. Live service pools
  and supervisor bindings enforce service/principal ownership and lifetime.
  Missing, expired, or completed bindings return `409`; a token can never
  recreate them. Temporary transport failures do not replay work or replace
  state. Browsers establish over HTTP and reuse the token as `?route=` for
  WebSockets. No route database, duplicate lease timer, or token refresh exists.
- Platform headers use lowercase `the8020-authorization`, `the8020-route`, and
  `the8020-internal-<purpose>`; cookies use `the8020_<purpose>`, including
  `the8020_auth`. Header lookup/stripping remains case-insensitive. Internal
  metadata must be stripped or regenerated at its trusted transport boundary.
- One sandbox has exactly one placement-group value and at most one allocation
  of a logical service; compatible different services may share it. A
  Workers-per-sandbox value of one is strict isolation; larger values are
  packing targets and may have small race-bound overshoot without exceeding the
  hard kernel-wide total-Worker limit. Scaling removes only excess idle Workers
  after Worker keepalive, retains configured warm sandboxes independently, and
  destroys ownerless sandboxes. Global allocation indexes are partitioned across
  enabled application-server nodes. Kernel packing policy has exactly one
  per-sandbox capacity dimension: total Workers, defaulting to 64. CPU and RAM
  have no settings, reservations, placement targets, admission limits, or cgroup
  ceilings; their raw usage is diagnostic only. Nodes enforce and advertise
  sandbox-count, Worker-count, and temporary-storage budgets. Insufficient
  capacity retains desired state, reports `PENDING_CAPACITY` or `DEGRADED`, and
  may spill new work through authenticated node forwarding.
- UUI establishment, messages, replay, heartbeat, reconnect behavior, program
  lifecycle, and session administration belong to the ordinary persistent UUI
  service handler. The Deno supervisor and Worker bridge must remain generic so
  another persistent protocol can reuse them without UUI-specific code.
- The sibling `uui` repository's `ui-config.json` is the sole current UUI
  configuration source. Its protocol, timing, limits, route paths, Home program,
  and Program terminated program are static package-owned constants until a
  package-owned configuration system is introduced; the kernel and generic
  runtime never validate, override, persist, or transport them.
- Administrative package commands are ordinary jobs running as `system`, using
  the existing service/job sandbox infrastructure and normal mounts, including
  dependencies in other packages. Never copy or snapshot package trees per
  command, add command-only mount overlays or isolation, or build a parallel
  execution path. These patterns turn a command into package-size filesystem
  work, defeat ordinary sandbox sharing, and duplicate capabilities the job
  system already owns. Fix missing execution capabilities in that shared owner.
- Ordinary service and job sandboxes mount the complete activated package tree
  read-only at `/workspace/packages`. Application durable shared state uses the
  kernel-owned database API; no generic package-data filesystem is mounted.
- Public services completely ignore platform tokens and execute as their
  configured user. They preserve raw cookies/headers for explicit package
  login/logout. Authenticated services verify the platform JWT in Go before
  request-triggered capacity or dispatch, then run users-package account/session
  policy inside the existing target Worker before HTTP handling or WebSocket
  acceptance.
- The kernel owns cryptographic integrity and private deployment key storage;
  the users Deno package owns passwords, sessions, revocation, login/logout, and
  application cookies. Context getters are synchronous and never authenticate.
  Do not introduce another auth service, Worker, sandbox, package copy, or
  parallel execution mechanism. Extend shared service/job capabilities at their
  owning layer when a real capability is missing.
- Services and jobs have the same `@the8020/kernel` API through their private
  Worker MessagePort and the same immutable invocation-scoped `@the8020/context`
  API. Every Worker starts with one validated user and outer service/job/program
  origin; authenticated requests use their authenticated user, while anonymous
  requests use the service's configured `anonymous_user` (default `system`).
  Jobs require an explicit or inherited user. Kernel-owned operations explicitly
  assign `system` when there is no caller. Every principal, including `system`,
  is a structural kernel identity independent of account rows. Creating,
  disabling, deleting, or losing a users table never changes execution
  eligibility. Deno users-package authentication alone applies
  enabled/password/session rules to interactive login. Package-local identities
  such as a UUI session remain package-owned. The trusted supervisor stamps
  execution identity and alone holds the per-sandbox token and node-private
  `/run/the8020/kernel.sock` access; application Workers cannot read either.
  Both workload types may use unrestricted outbound network and remote imports;
  only temporary and runtime-cache paths are writable by default.
- Active packages expose administrative commands through flat
  `cbus/commands/*.toml` and non-discoverable ordinary programs. Required
  `command` fields contain complete public names, independent of filenames or
  package paths; `program` names a same-package program. Duplicate public names
  fail validation, and nested command declaration folders are invalid. The
  kernel assembles one process-local immutable catalog from those files,
  refreshes it at startup and package lifecycle boundaries, and never persists
  the assembled catalog. Package command arguments remain raw strings.
- `the8020__packages__packages` is the authoritative desired and active package
  index. A fresh database stages the small source-owned bootstrap package list,
  evaluates and synchronizes every staged table in one job, runs package hooks,
  and publishes the package set before the service plane becomes ready. Normal
  boots validate the existing catalog without writes and do not rescan table
  modules. Package changes stage and validate only candidate packages,
  synchronize their schema, run pre/post activation hooks exactly once,
  atomically switch source, and refresh only affected services. Hook and
  deployment phases are durable and recoverable; PostgreSQL serializes
  deployment through an advisory lock.
- Global named secrets live only in `the8020__secrets__secrets`. Package records
  may retain one secret name but never its value. Kernel-owned Git operations
  resolve that value only for the selected package, inject it as a host-scoped
  transient HTTPS authorization header, and never write credentials into a
  repository URL or Git configuration. Administrative secret lists and writes
  omit stored values; only an explicit authenticated get returns one.
- The package-neutral Deno kernel SDK may invoke only explicitly registered
  JSON-in/JSON-out functions on one exact node, sandbox, and Worker. The kernel
  validates infrastructure identity, size, timeout, authentication, and
  forwarding, while function names and payloads remain opaque. A persistent
  handler may likewise report completion of its exact logical execution so the
  supervisor releases the binding; later use of its signed route returns `409`.
- Generic application execution has exactly service and job workloads. There is
  no separate user-session workload, session command family, session Worker
  adapter, or session restoration subsystem; authentication sessions, UUI
  application sessions, SSH connections, and the admin socket are distinct.
- `the8020/jobs` owns schedules, durable run history, and the Jobs admin
  program. Its prototype accepts positional JSON inputs, an execution user,
  sandbox group, and Any (default), All, or an exact node. UTC calendars combine
  explicit datetimes with a start date, selected months, month dates or
  weekdays, and multiple times. Calendar calculation, per-node cursors, claims,
  and history writes run in the package's Deno event listeners; jobs expose no
  scaling controls.
- Each kernel emits a local `minute` event at seconds 00 using its host clock.
  Generic events dispatch cached flat `events/*.toml` handler declarations
  asynchronously in parallel and return before program admission or completion.
  Each event declaration requires `event`, `description`, and a full
  `namespace/package/program` identifier in `program`; filenames do not select
  triggers. Deno emits through `kernel.events.emit`; the command bus exposes
  `kernel.events.emit <event> --data <json>`. Activation hooks use flat
  `hooks/*.toml` with required `hook`, `description`, and `program` fields;
  optional integer `order` defaults to zero and ties use declaration identity.
  Synchronous hooks support ordered chains, including `index-services`. One
  chain runs as one ordinary system job whose Deno dispatcher imports and awaits
  all handlers in the same Worker with shared mutable state. Normal sandbox
  mounts, permissions, grouping, and release-aware reuse apply; never copy
  packages or allocate separate execution machinery for each handler. One
  `kernel.reindex` entry point indexes commands, events, hooks, and services at
  boot, activation, and published source changes. Optional package IDs replace
  only selected package declarations; omission rebuilds the complete index.
  Executable source lives in ordinary programs. The complete `index-services`
  chain runs for each selected owning package even when handlers reside
  elsewhere. Scope is immutable; successful validated fragments publish
  atomically and remove omitted services, while failures preserve the previous
  fragment. Boot, activation, configuration edits, and cross-node invalidation
  share this path. Application failure must not block local command/job startup.
  `All` job schedules use independent node records; `Any` claims use short
  database transactions assigning the winning node ID. Direct targets are
  ignored by other nodes. Go owns events and sandbox execution, never job
  calendars.
- Node-local service records are recoverable runtime artifacts, never a global
  startup gate. Only current record shapes are accepted; unreadable or obsolete
  records move out of the live registry for diagnosis, and one service's decode,
  restore, or Worker failure must not prevent unrelated services or the
  package-service router from starting. Ordinary jobs retain only live
  in-process state and are never persisted, replayed, or added to automatic
  execution history. A fully retired service-pool record must be removed so an
  absent inherited sandbox cannot create an endless reconciliation failure after
  restart.
- Kernel development uses destructive schema evolution. Do not add compatibility
  adapters, persisted-data migrations, deprecated aliases, or dual sources of
  truth for old kernels; discard and initialize a fresh instance when a schema,
  settings key, runtime record, or node layout changes.
- Service configuration discovery belongs to Deno indexing at boot, activation,
  edits, or explicit reindex. Kernel reconciliation consumes the memory index.
  The maintenance timer touches only services with live runtime capacity or
  pending-capacity retries; service commands and cold requests reconcile their
  selected service directly.
- The kernel HTTP root `/` redirects to the safe relative 80|20 application path
  selected by the restart-required global `network.root_alias` setting,
  defaulting to `the8020/uui/shell/`; `/health` remains the independent plain
  kernel health endpoint.
- The kernel main HTTP listener binds all IPv4 interfaces so Docker and host
  port publication can reach it; administrative and internal runtime listeners
  remain private.
- A user's single development sandbox exposes the complete package tree as a
  writable gVisor-private overlay over shared packages. Explicit lifecycle
  boundaries checkpoint package deltas beneath `users/<username>/dev-sandbox/`;
  image-qualified writable system roots and root's home live beneath the same
  durable root. Lifecycle must never periodically poll or run a background
  filesystem scanner. Package content is scanned only by Git at an explicit
  lifecycle checkpoint, activation preview, or activation run. Publication
  creates package-level commits without pushing remotes, then recreates the
  process under the same deterministic sandbox identity to clear its private
  overlay.
- Development images keep Deno installed for developer commands but run no
  background runtime or filesystem scanner. Their `sandbox.sh` initializes the
  fresh runtime filesystem and replaces itself with `sleep`; persistence is a
  kernel-owned mount/storage property.
- Sandbox images and portable root filesystems must be materialized from pinned
  image manifests and must install their declared packages inside isolated
  image-build execution. Never copy host executables, libraries, package
  closures, certificates, or terminal data into a sandbox image; sandbox
  packages are not host prerequisites.
- Local sandbox debugging must use one generic authenticated loopback console
  WebSocket and direct PTY exec path for any running development or runtime
  sandbox. Every authenticated user is an administrator for this console until
  the full permission system replaces that temporary policy; do not add
  per-target restrictions or port forwarding in the interim.
- The Go kernel must expose password and authorized-public-key SSH on the
  runtime-mutable node-local `network.ssh_port`. Port replacement must bind
  before persistence, preserve established connections, and leave the current
  listener active on failure. It authenticates actual enabled 80|20 users, never
  persists or logs presented credentials, and defaults to creating or starting
  that user's development sandbox only after authentication. Public-key
  authentication reads the existing sandbox's bounded
  `/root/.ssh/authorized_keys` directly from its confined durable system root
  without creating or starting the sandbox. Ordinary remote commands execute
  through that sandbox's Bash login environment; commands beginning with the
  reserved `the8020 [sandbox-id=<dev-or-runtime-sandbox>]` grammar select a
  terminal target instead of executing. SSH uses the generic direct sandbox PTY
  path when requested and a byte-transparent process stream for non-PTY exec,
  forwarding all client behavior representable by that process/TTY boundary,
  including environment, commands, raw control/function-key bytes, resize,
  cancellation, distinct non-PTY stdout/stderr, real exit status, and canonical
  PTY EOF for half-closed streamed exec input. SSH-only forwarding channels and
  subsystems remain unavailable. SSH follows the same temporary
  all-authenticated-users administrator policy as the browser console.
- Runtime resource IDs must use a type prefix plus eight random lowercase
  alphanumeric characters: `sbx-` for sandboxes, `uis-` for UUI sessions, `rgp-`
  for runtime groups, and `wrk-` for Workers. Cleaned terminal sandboxes leave
  the live catalog immediately; metadata and bounded logs move to separately
  indexed history with node-local retention defaulting to seven days, and UUI
  presents that history separately.
- Keep opaque identities recognizable by a three-letter type prefix throughout
  APIs and diagnostics. When standardizing execution-context and node identity,
  use `ctx-` and `nod-`; reserve `job-` and `srv-` for actual job and service
  runtime instances. Declared objects retain their canonical names, displayed as
  `job:<name>`, `service:<name>`, or `program:<name>`. Identity changes belong
  to the owning shared contract; never invent log-only aliases or additional
  runtime objects merely to label logs.
- Authenticated session-shell theme preferences must remain browser-only:
  `sessionStorage` owns the current session override and `localStorage` seeds
  future tabs. The shell HTML starts dark and a CSP-nonced head initializer
  resolves a stored light preference or operating-system preference before CSS
  and first paint. Do not add theme fields, messages, APIs, or persistence to
  the UUI service, kernel, or another backend.
- The authenticated UUI theme toggle uses individually vendored Material sun and
  moon SVGs without visible mode text: dark mode shows the switch-to-light sun,
  light mode shows the switch-to-dark moon, and the accessible action label
  remains text.
- The authenticated UUI navbar brand is unboxed `80|20` text at `30px` and
  semibold weight: light-mode `80` uses `#cd9d00`, dark mode uses its brighter
  gold token, `20` uses the primary text color, and the pipe is a centered
  `24px` by `3px` rule rather than the font glyph.
- UUI screens use an explicit two-level visual hierarchy: unboxed top-level
  sections use H1 headings, and only second-level semantic field groups use
  rounded elevated surfaces. Box titles form a left-edge-aligned top-border tab
  with an open bottom: its left border continues through the tab to replace the
  covered box border, while its right border stops at the box top. Level-one
  page H1s keep `32px` before their layout when no description intervenes;
  descriptions keep `32px` before the layout, and section H1 margin plus layout
  gap also totals `32px` before field groups. Fields declare `short`, `medium`,
  or `long` responsive length, defaulting to `medium`, and incomplete rows
  expand without reordering fields or leaving avoidable gaps. Responsive
  placement snaps long fields to half-row boundaries and medium fields to
  quarter-row boundaries, so a half-width field can begin only at the start or
  midpoint of a wide row. Fields may declare a `rowSpan` from one through eight,
  defaulting to one; a spanning field reserves its complete responsive-grid
  rectangle, later fields advance in source order through legal free space, and
  holes remain rather than being backfilled through reordering. Spanning
  controls use one shared sibling-group row metric: an `N`-row field is exactly
  `N` standard label-and-control row heights plus `N - 1` 20px row gaps, so its
  underline meets the same boundary as the `N`th ordinary field. Textareas do
  not expose free-form resizing that can break those boundaries. Direct field
  labels and legends use the list-column header treatment: muted, uppercase,
  `0.7em`, weight `800`, with `0.06em` tracking; option labels retain body
  typography. Editable-field pencils align to the exact field end, with their
  visible Material glyph cropped to that edge; textarea pencils use the same
  underline-relative bottom offset as single-row controls. Select chevrons and
  native input affordances remain immediately before them. Section H1s, direct
  field-group card stacks, and wrapped card rows use one `24px` vertical-gap
  token, so the H1-to-first-card distance exactly matches the distance between
  subsequent cards; horizontal card gutters remain `16px`. Field-group content
  uses the title tab's exact `0.78rem` start and `1.5rem` end inline padding so
  their edges stay aligned.
- The authenticated UUI top bar permanently orders the unboxed `80|20` brand,
  shell-owned icon-only Back button, program-defined header controls/actions,
  and connection/theme controls. The Back control retains an accessible text
  label. The brand/Back cluster stays on the left and the connection/theme
  cluster stays flush right even when the dynamic middle is empty or hidden.
  `/p/the8020/uui/mod.ts` exports the reserved `BACK_EVENT`; programs compare
  against it and never declare their own Back action. Programs place non-Back
  controls and actions in `callScreen({ header: ... })`, not bottom action
  regions. The top bar always remains one row: as width shrinks, the dynamic
  middle keeps the largest stable leading prefix that fits and moves items from
  right to left into an overflow disclosure whose trigger sits immediately after
  that prefix and whose opened contents stack vertically. The open popover
  remains inside the viewport with a `10px` edge gutter, including on mobile
  widths.
- UUI-rendered button and text content supports validated vendored Material icon
  placeholders in the form `[[icon=<name>]]` with optional semantic or hex
  `color=<value>`. Inline icons are `1.2em` and vertically centered with their
  text; icons inside buttons are `1.5em`, and flex-based buttons keep `0.45em`
  between every rendered child, except explicitly sized icon-only shell
  controls. Only registered individual SVG assets and named color tokens may
  render; unknown icons and invalid colors remain literal text, and no icon font
  or unused collection is transferred.

## Child DOX Index

This root retains repository-wide contracts and files outside the child scopes
below.

- [defaults/AGENTS.md](defaults/AGENTS.md): first-run configuration/node-setting
  templates and the canonical generic runtime definition, source, image tooling,
  and pinned versions under `defaults/config/runtime/`.
- [kernel/AGENTS.md](kernel/AGENTS.md): the Go kernel architecture, authored
  source, declarative definitions, tests, and package-level DOX tree.

- Root-owned paths include `.vscode/`, `go.mod`, `go.sum`, `.go-version`,
  `.gitignore`, `install.sh`, `run.sh`, `release-tag.sh`, release resolver
  tests, and root-level project documentation.

# 80|20

- The product name is `80|20`; machine-safe package, protocol, and repository
  namespaces use `the8020`.
- The name comes from the Pareto 80:20 rule. Development prioritizes the small
  set of reusable capabilities that delivers most user value, then adds
  long-tail complexity only for concrete needs; this does not weaken
  correctness, security, or explicit contracts.
- Keep 80|20 clean, lean, fast, and direct: prefer native ownership and deletion
  over adapters, duplicate state, polling, or compensating patches. Runtime
  observations use owner-published absolute snapshots when that removes repeated
  inspection from hot paths. A hot or periodic path may not scan an unbounded
  collection, retain unbounded output, or hold a broad lock across filesystem,
  process, or network I/O.
- Treat the component where an error surfaces as evidence, not automatically as
  the fix scope. Establish ownership from the contract and full data flow before
  changing code.
- Diagnose failures through their full ownership boundary and repair the shared
  contract at its owning layer. Do not add caller-specific coercions, retries,
  fallbacks, compatibility shims, or duplicate logic to mask a shared defect;
  change a caller only when it violates an already-correct contract, and prove
  the root repair with both a focused regression and the affected end-to-end
  path.
- we are building a runtime platform for companies and developers
- the platform's purpose is to replace individually hosted and managed
  microservices with one system
- the system will have a centralized database and decentralizes execution nodes
- execution nodes will run their static kernel in Go and core+custom application
  code in Deno, kernel manages file access, network, processes, sandboxing and
  application code contains services, screens, jobs etc

## Development Workflow

- Verify the final release contents, including documentation added to discovery
  folders, before tagging. Fresh-start verification must exercise the Docker
  entrypoint's first-user path; a browser flow that creates its own test user
  does not verify that path. Report Docker build/run coverage separately from
  native runtime tests when Docker is unavailable.

- `.vscode/` configures the Deno language server and linting for
  `defaults/config/runtime/deno/`, recommends the Go/Deno/TOML/shell development
  extensions, and exposes `Run 80|20` as an interactive `./run.sh` launch
  configuration.
- `install.sh` owns platform build and runtime-image freshness. It provisions a
  checksum-verified project-local Go toolchain and node-local gVisor, generates
  the generic protocol, builds exactly `kernel` and `admin`, initializes the
  selected instance layout when absent, atomically refreshes platform-owned
  `node/kernel/runtime/definitions/` and read-only development helper scripts,
  and materializes verified service and development images under
  `node/kernel/runtime/images/`. Complete generic image-input digests make
  unchanged installs fast; required packages and Deno bundling execute inside
  the pinned gVisor image build. The default verification gate runs Go and
  generic runtime checks only. `--skip-runtime-host` prevents full-mode host
  mutation while retaining rootless gVisor, and `--skip-verification` skips only
  test gates. Installation checks Git and stages the source-owned bootstrap
  package set as clean Git repositories only for a fresh fixed-layout instance;
  first kernel boot publishes it transactionally in the database. It never
  builds, formats, lints, type-checks, or tests application packages and never
  runs a UUI build or browser E2E. `THE8020_RELEASE_VERSION=<major.minor>` is an
  installer-only release input: it disables sibling-source snapshots and stages
  every bootstrap package from its compatible resolved Git tag.
- `run.sh` may be invoked from any directory. It treats that current directory
  as the instance root and runs `install.sh --skip-verification`, which
  refreshes both binaries, initializes the default layout when absent, and
  refreshes only changed generic images before the kernel starts. The kernel
  never builds an image or executes Deno during startup. When a kernel is
  already running, `run.sh` gracefully restarts it after the rebuild, waits for
  the replacement control plane, then attaches without owning its lifecycle;
  otherwise it starts a kernel. The interactive admin remains open after
  `kernel.restart` or `kernel.shutdown`; kernel restart gracefully exec-replaces
  the process itself and therefore works for both attached and wrapper-started
  kernels. When the admin exits and `run.sh` owns the still-live kernel, the
  wrapper polls the command socket to render in-place nine-stage graceful
  shutdown progress before bounded TERM/KILL escalation. It waits without a
  fixed deadline only for the control-plane socket while the kernel process
  remains alive; runtime recovery never gates the console.
  `THE8020_SKIP_RUNTIME_HOST=true` forwards the rootless-only install mode.
- The source workspace `/workspace/8020/` contains sibling package repositories
  `admin-core`, `admin-db`, `db`, `demo`, `dev-core`, `jobs`, `packages`,
  `secrets`, `services`, `system`, `users`, and `uui`, alongside `kernel`; the
  kernel repository contains no source package copies. Each package owns its
  formatting, linting, type checking, tests, browser bundles, release, and
  activation readiness. Initialized instances clone indexed repositories into
  their mapped `packages/<namespace>/<repository>/` tree, which service
  sandboxes mount read-only.
- The sibling `the8020/branding` repository owns the shared brand assets and
  editable logo sources; branding assets live outside the kernel repository.
- The sibling `admin-core` repository's `programs/packages` program lists
  canonical package IDs through the cheap package summary command. Its
  selected-package detail alone performs bounded service/program/file inspection
  and independent Git repository inspection; it can pull, push, check out a
  branch or commit, select a stored secret name, and links contained services
  into the shared service detail program. Its separate Secrets program lists
  names/timestamps and creates or overwrites values without ever loading a
  stored value into the edit screen.
- Every mapped `packages/<namespace>/<repository>/` root is an independent Git
  repository; there is no master source repository. Developers edit a private
  sandbox overlay whose durable state is confined to
  `users/<username>/dev-sandbox/`, and typed activation is the only path that
  commits changes into shared package roots. Activation creates one commit per
  selected changed package, retains root's home and installed system changes,
  and never pushes configured remotes.
- The platform-owned instance `scripts/` tree is mounted read-only and
  executable at `/workspace/scripts` in development sandboxes. Its `activate`
  helper calls the same typed activation path as UUI, requires a commit message
  for publication, and defaults Git author identity to the authenticated
  username. Opt-in `install-codex.sh` and `install-claude.sh` helpers install
  each vendor's latest native CLI into persistent root storage and configure its
  unattended full-access mode without login or other preferences. Every commit
  ends with valid TOML activation metadata. The development UUI previews changed
  packages plus file/add/remove counts and activates all ready changes together.
- The sibling `uui` repository owns the default Home and Program terminated
  programs selected by static `ui-config.json`; the UUI handler loads them
  through the ordinary validated program path without kernel configuration. Home
  derives its launcher rows by rescanning program manifests declaring both
  `uui = true` and `discoverable = true` in the mounted package tree before each
  render and on its explicit Refresh action; program IDs are never hardcoded
  into Home. The separate `the8020/admin-core/programs` catalog includes all
  ready programs and opens the shared Jobs execution form for custom inputs. UUI
  returns do not produce generic program-completion notifications.
- The sibling `dev-core` repository's `programs/development-test` controls the
  authenticated user's development sandbox and selects it for the generic
  browser Bash console. Development sandboxes include APT/dpkg with official
  Debian or Ubuntu repositories in both portable and rootful modes; bounded
  container-root capabilities and an image-qualified native durable system root
  support persistent interactive package installation. The portable and full
  runtime images include Bash for this authenticated-user debug path.
- Each initialized instance has fixed top-level `packages/`, `users/`,
  `database/`, `scripts/`, and `node/` roots plus one `kernel.toml`. Shared
  settings, users, authentication sessions, package state, service policy and
  versions, topology, and named secrets are ordinary package tables in the
  system database. `users/<username>/` owns that user's durable filesystem data;
  the private top-level `database/` owns the default single-node SQLite system
  database and is never sandbox-mounted. SQLite uses WAL so readers can run
  alongside its single writer; each kernel owns a dynamically opened
  `database/sql` pool with runtime-mutable node-local open/idle limits
  defaulting to 32/8 and exposes pool pressure through `kernel.status`.
  Application SQL is admitted up to two connections below the configured open
  maximum when the pool is large enough, preserving kernel-owned database
  progress. `node/` owns every observed or ephemeral node-local artifact.
  Per-node settings stay in `kernel.toml`, and credentials or configuration are
  never mounted into sandboxes.
- Source-owned commands and development toolchains remain repository-local;
  installation places each rootless node's pinned runsc and images under its own
  `node/kernel/`. Full-mode Phase 1B host dependencies are the only
  global-install exception and require detected `SYS_ADMIN`, `NET_ADMIN`, and
  writable cgroup-v2 authority; unsupported environments must fail clearly and
  never receive an unsandboxed Deno fallback.
- Tracked generic Deno execution code, protocol source, portable image-build
  tooling, and image-installed development helpers belong under
  `defaults/config/runtime/` and are refreshed into initialized
  `node/kernel/runtime/definitions/`. Source-tree `.development/` holds only
  disposable framework-development toolchains, downloads, caches, generated
  build glue, and test instances; no initialized instance reads it as runtime
  state. Materialized images, build caches, downloads, smoke records, and
  temporary construction output live under `node/kernel/runtime/`, with images
  beneath `node/kernel/runtime/images/`.
- Live sandbox state belongs under `node/kernel/runtime/groups/`; terminal
  metadata, log tails, retained-ID markers, and time-partitioned indexes belong
  under the separate `node/kernel/runtime/sandbox-history/` root and never enter
  live scans.
- Kernel restart is control-plane-first: administration must become available
  before sandbox cleanup or provisioning, inherited sandboxes are destroyed
  without health restoration by default, and replacement capacity is created
  lazily from explicit workload demand.
