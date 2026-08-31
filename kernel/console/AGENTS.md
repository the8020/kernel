# Purpose

- Broker lifecycle-tracked interactive processes in running 80|20 sandboxes
  and bridge the authenticated local browser WebSocket transport to them.

# Ownership

- Own target-provider selection, transport-neutral active process leases and bounds,
  provider-withdrawal cleanup, plus the generic console WebSocket protocol,
  same-origin/authentication gate, byte relay, resize relay, and connection
  cleanup.
- Do not own sandbox lifecycle, authorization policy, terminal rendering,
  command presentation, or backend-specific PTY creation.

# Local Contracts

- `/_the8020/console` is loopback-only through the main kernel listener and
  requires the ordinary opaque authentication cookie and
  `the8020.console.v1` subprotocol.
- Any authenticated user may currently open a console in any selected running
  runtime or development sandbox. Granular authorization is deferred to the
  full permission system.
- `OpenConsole` is the shared kernel transport boundary used by WebSocket and
  SSH. Every returned PTY or direct process stream is registered until close, and broker/provider
  shutdown closes it regardless of transport.
- Broker leases preserve an attached backend process's real exit status for
  transports such as SSH and preserve its separate stderr stream.
- The first frame selects a bounded target, direct argument vector, environment,
  working directory, and terminal size. Later binary frames are input; text
  frames are resize controls; server binary frames are output.
- Console sessions expose no password, SSH key, sandbox token, host port, or
  port-forwarding capability.

# Work Guidance

- Keep the broker transport-only and use provider/backend interfaces for all
  target, stream, and PTY behavior.

# Verification

- Unit tests cover authentication, same-origin/subprotocol enforcement, target
  selection, transport-neutral leases, binary relay, resize, malformed/bounded
  frames, provider removal, and close cleanup. Real rootless/browser/SSH tests
  prove Bash PTY behavior.

# Child DOX Index
