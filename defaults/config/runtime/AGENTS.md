# Purpose

- Define the canonical package-neutral Deno service/job runtime and the
  separate development-sandbox image.

# Ownership

- Own pinned runtime versions and checksums, generic protocol source, the Deno
  supervisor and Worker bootstrap, generic HTTP/WebSocket and kernel-capability
  SDKs, image definitions, portable/full materialization, and runtime-specific
  tests.
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
- Stateless/persistent service pools, persistent execution bindings,
  keep-alive, exact-Worker reuse, explicit persistent completion, physical
  WebSocket relay, and registered JSON-in/JSON-out Worker functions are generic
  capabilities. Function names and application payloads remain opaque.
- `versions.toml` and `protocol/schema.json` are authoritative. Generation
  writes `protocol/generated.ts`, build-only Go output, and the tracked Go
  mirror under `kernel/runtime/protocol/`; generated files are not hand-edited.
- `install.sh` refreshes this tracked tree into each instance's
  `node/kernel/runtime/definitions/`, hashes the complete generic image input set before build,
  and publishes only verified artifacts under
  `node/kernel/runtime/images/`. Unchanged verified digests are reused.
- Deno dependency preparation and generic HTTP bundling execute inside the
  pinned isolated image build after a digest miss. Normal startup has no
  host-side Deno or image-build process, and the Go kernel never invokes these
  scripts.
- Portable construction materializes the pinned OCI base without copying host
  executables, libraries, package metadata, certificates, or terminal data.
  Declared packages install inside rootless gVisor on an ordinary host; during
  construction of the enclosing Docker image they install through `chroot`
  inside that existing isolated build sandbox, avoiding a forbidden nested
  user namespace. Full construction uses the same staged generic runtime and
  pinned image definition through BuildKit when host authority exists.
- Portable installation publishes the complete pinned gVisor execution payload:
  `runsc` and every release-provided `gvisor-bin/` companion remain adjacent
  under `node/kernel/bin/` so runtime startup never downloads missing helpers.
- The service image runs non-root and includes only pinned Deno, generic runtime
  modules/protocol, the pinned Kysely dependency used by the database SDK, and
  explicitly required administrator debugging tools.
  `stage-service-runtime.sh` excludes tests, DOX files, examples, application
  source, and unrelated files.
- Service and job supervisors may run only the pinned Deno binary for module
  validation; nested application Workers do not inherit subprocess permission.
- The node-private runtime callback directory is bind-mounted at
  `/run/the8020`; supervisors connect to `kernel.sock` afresh for every HTTP/JSON
  call so kernel socket replacement is transparent. The canonical runsc
  configuration permits opening existing host Unix sockets, but not creating
  them; only explicitly mounted sockets are reachable. Deno receives read/write
  permission for the exact socket path because its Unix connect API requires
  both, while the mounted directory remains read-only.

# Work Guidance

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
- Portable verification launches the staged rootfs as UID/GID 1993 through the
  pinned rootless runsc and imports the generic HTTP and kernel modules before
  publishing image and smoke records. An enclosing Docker build verifies the
  same modules as UID/GID 1993 inside its build sandbox, records that narrower
  provenance, and the container entrypoint replaces it with a real pinned-runsc
  smoke record before kernel startup. Full verification imports and launches
  the canonical image when host authority is available.

# Child DOX Index

- `cni/AGENTS.md`: canonical full-mode CNI template.
- `protocol/AGENTS.md`: versioned generic control schema and generation.
- `deno/AGENTS.md`: generic supervisor, Worker bootstrap, SDKs, examples, and
  Deno verification.
- `development/AGENTS.md`: separate development image and materialization.
