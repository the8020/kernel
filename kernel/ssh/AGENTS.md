# Purpose

- Expose authenticated SSH terminal access to 80|20 sandboxes.

# Ownership

- Own the SSH listener, persistent node host key, password-authentication
  adapter, fixed remote selector grammar, SSH session-channel protocol, and PTY
  byte/resize/signal relay.
- Do not own users, authorization roles, development-workspace lifecycle,
  sandbox lifecycle, shell command interpretation, or PTY creation.

# Local Contracts

- `network.ssh_port` is the restart-required node-local listener port. The
  Ed25519 host key lives only at `node/kernel/ssh/host_ed25519`, mode `0600`
  below a mode-`0700` directory, and is generated once from `crypto/rand`.
- SSH accepts only password authentication against real enabled 80|20 users.
  Presented password bytes are never persisted or logged and are cleared after
  verification. Connections, sessions, authentication attempts, handshakes,
  selector payloads, and terminal geometry are bounded.
- Session diagnostics log authenticated identity, request/stage, target, and
  command byte length, but never passwords or remote command contents.
- Bounded standard `env` requests forward every valid client variable unchanged
  into the sandbox process. Kernel defaults for `TERM`, `PATH`, `HOME`, `SHELL`,
  `USER`, and `LOGNAME` apply only when the client does not provide them; the
  default `PATH` includes the standard administrative `sbin` directories.
- A shell request opens the authenticated user's deterministic default
  development workspace, creating or starting its sandbox when needed.
- An ordinary exec request runs through `[/bin/bash, -lc, <command>]` inside the
  authenticated user's default development sandbox. Commands beginning with
  reserved `the8020` use the structured `the8020 [sandbox-id=<id>]` selector
  grammar instead; its optional parameter accepts only a canonical `sbx-`
  sandbox ID. Exact current-manager ownership selects the development provider;
  every other valid ID selects the runtime provider, and malformed selector
  parameters are rejected.
- Only SSH `session` channels are accepted. Port, agent, X11, and socket
  forwarding and subsystems are unavailable.
- Shell and selector sessions launch `[/bin/bash, -l]`; ordinary exec requests
  launch `[/bin/bash, -c, <command>]` through the shared console broker and
  return the sandbox process's actual exit status. A client-requested PTY uses
  the sandbox PTY path; exec without `pty-req` uses a byte-transparent process
  stream with distinct stdout and SSH extended-data stderr, real stdin
  half-close semantics, and real exit status. Raw bytes, window changes, and
  terminal cancellation signals are relayed to the sandbox PTY. SSH stdin EOF is
  relayed as canonical PTY EOF so streamed exec consumers such as `bash -s`
  terminate normally; closing SSH ends only that console process.
- The adapter forwards all behavior representable by the sandbox process or PTY
  boundary. SSH-only forwarding channels and subsystems are not process behavior
  and remain unavailable without a sandbox-side SSH protocol endpoint.
- The current temporary authorization policy permits every authenticated user to
  select any running sandbox.

# Work Guidance

- Keep the reserved selector grammar parsed structurally and execute ordinary
  commands only inside the selected sandbox. Never place authentication secrets
  in errors, logs, permissions metadata, or session state.

# Verification

- Package tests cover persistent host-key safety, real SSH password handshakes,
  default and selected routing, bounded environment forwarding, ordinary sandbox
  commands and exit statuses, streamed exec stdin and EOF, rejected channels,
  PTY geometry and resizing, exact control/escape-byte relay, and shutdown.
- The real rootless development E2E exercises SSH through the actual
  authentication, development-workspace, console-broker, and runsc PTY path,
  including a contextual working-directory prompt and a plain-`xterm` Nano
  full-screen session.

# Child DOX Index
