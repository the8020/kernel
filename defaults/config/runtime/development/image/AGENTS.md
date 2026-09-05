Parent DOX: [kernel/defaults/config/runtime/development DOX](../AGENTS.md).

# Purpose

- Define the source-controlled helper payload installed into development images.

# Ownership

- Own files installed beneath `/opt/development` and the image's `/etc/profile`.

# Local Contracts

- The development image payload is separate from the service image definition,
  contains no credentials or package source, and starts `sandbox.sh`, which
  restores Debian's standard lock directory in the fresh `/run` tmpfs and
  becomes one inert `sleep` process. Deno remains available only for developer
  commands.
- The image-owned `/etc/profile` sources Debian's interactive Bash defaults and
  sets a restrained colorized `user@host:working-directory` prompt, with a
  plain-text fallback for terminals without common ANSI capabilities.
- APT/dpkg is part of both rootful and portable image materializations. Both
  include `clear`, common `ncurses-base` terminal definitions including `xterm`
  and `xterm-256color`, official repository metadata, and no host credentials.
- APT and shell package operations invoke Debian's native `/usr/bin/dpkg`
  directly. The image contains no dpkg wrapper or filesystem-metadata
  compatibility layer.
- The image contains no filesystem scanner, draft applier, activation helper,
  persistence helper, or Go compiler. Durable system storage, private package
  overlay checkpoints, and the separate `/workspace/scripts` helper mount are
  entirely kernel-owned.

# Work Guidance

- Keep helpers limited to image-provided Bash and Debian tools and keep idle
  lifecycle free of Deno or filesystem traversal.

# Verification

- Deno formatting/type checks cover `activate.ts`; image assembly and real
  sandbox tests verify tools, both supported xterm definitions, a contextual
  prompt that tracks the working directory, a real Nano full-screen SSH session,
  a native dpkg transaction plus new-directory installation, a usable Debian
  lock directory, ephemeral `/run`, a Deno-free `sleep` idle process, absence of
  a scanner, shell editing, preview, activation, and native restart persistence.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
