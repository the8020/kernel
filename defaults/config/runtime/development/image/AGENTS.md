# Purpose

- Define the source-controlled helper payload installed into development images.

# Ownership

- Own files installed beneath `/opt/development`, `/usr/local/bin`, and the
  image's `/etc/profile`.

# Local Contracts

- The development image payload is separate from the service image under
  `config/runtime/`, contains no credentials or package source, and
  starts `sandbox.sh`, which restores Debian's standard lock directory in the
  fresh `/run` tmpfs and becomes one inert `sleep` process. Deno remains
  available only for developer commands.
- The image-owned `/etc/profile` sources Debian's interactive Bash defaults and
  sets a restrained colorized `user@host:working-directory` prompt, with a
  plain-text fallback for terminals without common ANSI capabilities.
- APT/dpkg is part of both rootful and portable image materializations. Both
  include `clear`, common `ncurses-base` terminal definitions including
  `xterm` and `xterm-256color`, official repository metadata, and no host
  credentials.
- APT and shell package operations invoke Debian's native `/usr/bin/dpkg`
  directly. The image contains no dpkg wrapper or filesystem-metadata
  compatibility layer.
- The image contains no filesystem scanner, draft applier, persistence helper,
  or Go compiler. Native durable workspace storage is entirely kernel-owned.
- The `activate` wrapper grants only injected workspace variables and loopback
  networking to `activate.ts`; it calls the authenticated workspace endpoint,
  which dispatches the ordinary typed activation command.

# Work Guidance

- Keep helpers limited to image-provided Bash and Debian tools and keep idle
  lifecycle free of Deno or filesystem traversal.

# Verification

- Deno formatting/type checks cover `activate.ts`; image assembly and real
  sandbox tests verify tools, both supported xterm definitions, a contextual
  prompt that tracks the working directory, a real Nano full-screen SSH
  session, a native dpkg transaction plus new-directory
  installation, a usable Debian lock directory, ephemeral `/run`, a Deno-free
  `sleep` idle process, absence of a scanner, shell editing, preview,
  activation, and native restart persistence.

# Child DOX Index
