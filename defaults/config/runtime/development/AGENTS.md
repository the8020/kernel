# Purpose

- Own the separate editable development sandbox image and its portable
  rootless/full materialization tooling and image payload.

# Ownership

- Own helper/runtime files, rootful materialization, portable rootless assembly,
  and pinned developer-tool identity. Each initialized system receives the
  platform-owned definition under `node/kernel/runtime/definitions/development/`; materialized
  rootfs and records live under
  `node/kernel/runtime/images/development/`.
- Do not own sandbox storage, package Git commits, activation, or developer
  files.

# Local Contracts

- The image contains Git, pinned Deno, Bash, common Unix tools, `clear`, the
  common `ncurses-base` terminal definitions including `xterm` and
  `xterm-256color`, CA certificates, curl, Nano, and APT/dpkg. The canonical
  `activate` helper is supplied separately through the read-only
  `/workspace/scripts` mount. Codex, Node.js, and npm are not
  preinstalled; developers may add their own tools through APT.
- Interactive login shells source the image-owned `/etc/profile` and expose a
  restrained colorized `user@host:working-directory` prompt, with a plain-text
  fallback for terminals without common ANSI capabilities. The console PATH
  includes root's native user-binary directory so tools installed by mounted
  helpers are immediately runnable by name.
- APT and shell `dpkg` use Debian's native binaries directly, without an
  image-owned compatibility wrapper or metadata emulation. Durable system-root
  storage must support native Linux ownership and mode semantics.
- `sandbox.sh` restores Debian's standard lock directory in the fresh `/run`
  tmpfs, then replaces Bash with `/usr/bin/sleep infinity`. Images contain no
  draft/apply/snapshot helper and perform no background filesystem traversal.
- Rootful mode exports the editable `Containerfile` through the provisioned host
  BuildKit; rootless mode materializes the same pinned OCI base and installs the
  declared packages inside a temporary gVisor image-build sandbox. When the
  platform itself is built inside Docker, the already isolated build sandbox
  supplies that boundary through `chroot` without a nested gVisor launch.
  No mode imports host binaries or host package closures, and development
  sandboxes run behind the direct runsc driver.
- The image is an immutable template only. The kernel copies the current image
  into an image-qualified native durable root on initial sandbox creation or
  confirmed factory reset, then retains that recorded private root across later
  image updates. Interactive APT/dpkg and system changes therefore require no
  overlay or snapshot.
- `install.sh` rebuilds the image only when its complete generic input digest
  changes. Kernel startup only reads the verified record; there is no image
  plugin or lifecycle persistence helper.

# Work Guidance

- Keep installation explicit, helpers limited to image-provided Bash and Debian
  tools, idle lifecycle limited to `sleep`, and persistence out of image code.

# Verification

- Portable installation verifies required executables, the absence of Codex,
  Node.js, and npm, APT metadata, Debian `dpkg`, common terminfo, pinned tools,
  and image identity. The real
  rootless/rootful E2E requires a contextual working-directory prompt, a
  native dpkg transaction, an actual `xterm` full-screen Nano session over
  SSH, a `sleep` PID 1, a usable Debian lock directory, ephemeral `/run`, no
  scanner, durable root-home/APT writes and private package deltas across
  sandbox restart, mounted-helper preview/activation, and verified source/factory reset
  boundaries.

# Child DOX Index

- `image/AGENTS.md`: source-controlled in-image helper payload.
