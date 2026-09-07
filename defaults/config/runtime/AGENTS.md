Parent DOX: [kernel/defaults DOX](../../AGENTS.md).

# Purpose

- Define the canonical package-neutral Deno service/job runtime and the separate
  development-sandbox image.

# Ownership

- Own pinned runtime versions and checksums, generic protocol source, the Deno
  supervisor and Worker bootstrap, generic HTTP/WebSocket, kernel-capability,
  and immutable execution-context SDKs, image definitions, portable/full
  materialization, and runtime-specific tests.
- The Go kernel owns backend selection, sandbox/network/resource/mount policy,
  placement, opaque persistent routing, the system database connection, and
  node-local runtime state.
- Application packages own every application protocol and behavior. This tree
  must contain no UUI implementation, package tests, package build products, or
  hardcoded application identity.

# Local Contracts

- Generic application workloads are exactly `service` and `job`. Development
  sandboxes use a separate image with `sleep` as init and do not host the
  supervisor.
- Every service/job sandbox has one infrastructure supervisor and zero or more
  Workers. Entrypoints load only inside Workers; package/runtime files stay
  read-only and only temp/cache paths are writable. Both workloads have
  unrestricted outbound network and remote imports without using Deno
  `--allow-all`.
- Stateless/persistent service pools, persistent execution bindings, keep-alive,
  exact-Worker reuse, explicit persistent completion, physical WebSocket relay,
  and registered JSON-in/JSON-out Worker functions are generic capabilities.
  Function names and application payloads remain opaque.
- `versions.toml` and `protocol/schema.json` are authoritative. Generation
  writes `protocol/generated.ts`, build-only Go output, and the tracked Go
  mirror under `kernel/runtime/protocol/`; generated files are not hand-edited.
- `install.sh` refreshes this tracked tree into each instance's
  `node/kernel/runtime/definitions/`, hashes the complete generic image input
  set before build, and publishes only verified artifacts under
  `node/kernel/runtime/images/`. Unchanged verified digests are reused.
- Deno dependency preparation and generic HTTP bundling use the pinned image's
  Deno inside isolated image-build execution after a digest miss. Normal startup
  has no host-side Deno or image-build process, and the Go kernel never invokes
  these scripts.
- Portable construction materializes the pinned OCI base without copying host
  executables, libraries, package metadata, certificates, or terminal data.
  Declared packages install inside rootless gVisor on an ordinary host; during
  construction of the enclosing Docker image they install through `chroot`
  inside that existing isolated build sandbox, avoiding a forbidden nested user
  namespace. Deno compilation and type verification run outside that chroot in
  the enclosing build container, where `/proc/self/maps` is available, using the
  staged image's Deno binary and keeping output/cache in the staged image. The
  subsequent non-root chroot smoke verifies installed runtime imports. Full
  construction uses the same staged generic runtime and pinned image definition
  through BuildKit when host authority exists.
- Portable installation publishes the complete pinned gVisor execution payload:
  `runsc` and every release-provided `gvisor-bin/` companion remain adjacent
  under `node/kernel/bin/` so runtime startup never downloads missing helpers.
- The service image runs non-root and includes only pinned Deno, generic runtime
  modules/protocol, the pinned Kysely dependency used by the database SDK, the
  pinned Zod dependency used by the HTTP self-types, and explicitly required
  administrator debugging tools. `stage-service-runtime.sh` excludes tests, DOX
  files, examples, application source, and unrelated files.
- `stage-service-runtime.sh --sources` lists production TypeScript recursively,
  excluding `examples/`, `test/`, and `*_test.ts`. Staging and both image hashes
  consume that same list, so added modules and nested sources participate
  automatically. HTTP sources stage under `http-source/` for bundling. Full
  image construction copies the complete staged `runtime/` directory.
- The image import map exposes the activated read-only package tree through the
  single `/p/` prefix. Package imports include their namespace, package, file,
  and extension, for example `/p/the8020/db/mod.ts`. Never add package-specific
  runtime mappings.
- Service and job supervisors may run only the pinned Deno binary for module
  validation; nested application Workers do not inherit subprocess permission.
- The node-private runtime callback directory is bind-mounted at `/run/the8020`;
  supervisors connect to `kernel.sock` afresh for every HTTP/JSON call so kernel
  socket replacement is transparent. The canonical runsc configuration permits
  opening existing host Unix sockets, but not creating them; only explicitly
  mounted sockets are reachable. Deno receives read/write permission for the
  exact socket path because its Unix connect API requires both, while the
  mounted directory remains read-only.
- The same mount exposes logs.sock. Ordinary logs use a separate persistent
  framed connection directly to logd, authenticated once with the sandbox token.
  Supervisors hold exact socket permissions; application Workers have only their
  private MessagePort and receive neither socket access nor credentials.

# Work Guidance

- Treat the generic Deno runtime as part of the protected kernel foundation. Add
  a capability only when existing programs, services, jobs, hooks, events, and
  bridge contracts cannot provide it; keep application modules and dependencies
  in their own packages.
- Verify necessary changes through the shared Go/Deno contract and the affected
  workload, including identity, cancellation, stream bounds, and cleanup.
  Package-specific semantics must remain opaque to the runtime.

- Deno 2.9 Unix connect requires read/write and unix:<absolute-path> network
  permission. The shared process argument owner supplies all three exact grants
  for kernel.sock and logs.sock, including under restricted egress profiles.

- Keep modules small, strict, generic, and free of application branching. Use
  Web Workers, transferable streams, explicit permissions, structured control
  envelopes, and bounded diagnostics.
- Portable mode must not mutate the host. Full host installation requires
  detected Linux root authority with `SYS_ADMIN`, `NET_ADMIN`, and writable
  cgroup v2.

# Verification

- Deno formatting, linting, type checking, and tests cover supervisor/Worker
  lifecycle, service/job contracts, streaming, persistent binding/completion,
  exact registered Worker invocation, cancellation, permissions, and crashes.
- `bundle-runtime.sh` checks the published HTTP self-types and complete Zod API
  against the image's pinned dependency using only cached dependencies before
  either runtime image can be published. Dependency installation consumes the
  image's import map and frozen lockfile, without a second dependency/version
  list in the build script.
- `bash defaults/config/runtime/stage-service-runtime_test.sh` verifies that a
  newly added nested module is staged and hashed while tests, examples, and DOX
  stay out of the image.
- Portable verification launches the staged rootfs as UID/GID 1993 through the
  pinned rootless runsc and imports the generic HTTP, kernel, and context
  modules before publishing image and smoke records. An enclosing Docker build
  verifies the same modules as UID/GID 1993 inside its build sandbox, records
  that narrower provenance, and the container entrypoint replaces it with a real
  pinned-runsc smoke record before kernel startup. Full verification imports and
  launches the canonical image when host authority is available.

# Child DOX Index

- [cni/AGENTS.md](cni/AGENTS.md): canonical full-mode CNI template.
- [deno/AGENTS.md](deno/AGENTS.md): generic supervisor, Worker bootstrap, SDKs,
  examples, and Deno verification.
- [development/AGENTS.md](development/AGENTS.md): separate development image and
  materialization.
- [protocol/AGENTS.md](protocol/AGENTS.md): versioned generic control schema and
  generation.
