# Purpose

- Own the separate editable development sandbox image and its portable
  rootless/full materialization tooling and image-installed helpers.

# Ownership

- Own helper/runtime files, rootful materialization, portable rootless assembly,
  and pinned developer-tool identity. Each initialized system receives the
  platform-owned definition under `config/runtime/development/`; materialized
  rootfs and records live under
  `node/kernel/runtime/images/development/`.
- Do not own workspace storage, package Git commits, activation, or developer
  files.

# Local Contracts

- The image contains Codex CLI, Git, pinned Deno, Bash, common Unix tools,
  `clear`, the common `ncurses-base` terminal definitions including `xterm`
  and `xterm-256color`, CA certificates, curl, Nano, APT/dpkg, and the
  workspace-scoped `activate` helper.
- Interactive login shells source the image-owned `/etc/profile` and expose a
  restrained colorized `user@host:working-directory` prompt, with a plain-text
  fallback for terminals without common ANSI capabilities.
- APT and shell `dpkg` use Debian's native binaries directly, without an
  image-owned compatibility wrapper or metadata emulation. Durable system-root
  storage must support native Linux ownership and mode semantics.
- `keepalive.sh` immediately replaces Bash with `/usr/bin/sleep infinity`.
  Images contain no draft/apply/snapshot helper and perform no background
  filesystem traversal.
- Rootful mode exports the editable `Containerfile` through the provisioned host
  BuildKit; rootless mode materializes the same pinned OCI base and installs the
  declared packages inside a temporary gVisor image-build sandbox. Neither mode
  imports host binaries or host package closures. Both modes run behind the
  direct runsc development driver.
- The image is an immutable template only. The kernel copies it once into an
  image-qualified native durable root per developer and runs that private root
  directly; interactive APT/dpkg and system changes therefore require no
  overlay or snapshot.
- `install.sh` rebuilds the image only when its complete generic input digest
  changes. Kernel startup only reads the verified record; there is no image
  plugin or lifecycle persistence helper.

# Work Guidance

- Keep installation explicit, helpers limited to image-provided Bash and Debian
  tools, idle lifecycle limited to `sleep`, and persistence out of image code.

# Verification

- Portable installation verifies required executables, APT metadata, Debian
  `dpkg`, common terminfo, pinned tools, and image identity. The real
  rootless/rootful E2E requires a contextual working-directory prompt, a
  native dpkg transaction, an actual `xterm` full-screen Nano session over
  SSH, a `sleep` PID 1, no scanner, durable source/home/APT writes and package
  directories across sandbox restart, helper preview/activation, and verified
  source/factory reset boundaries.

# Child DOX Index

- `image/AGENTS.md`: source-controlled in-image helper payload.
