# Purpose

- Open one interactive PTY or byte-transparent attached process through runsc.

# Ownership

- Own console-socket creation, descriptor receipt, PTY resize, attached stream
  half-close, runsc exec process cleanup, and the shared direct-runsc process
  implementation.
- Do not select sandbox targets, authorize users, terminate WebSockets, or own
  sandbox lifecycle.

# Local Contracts

- `Open` accepts an already validated runsc exec argument vector, inserts a
  private temporary console socket, and returns a bounded generic backend
  console. `OpenConfigured` additionally applies one caller-owned process
  configurator immediately before runsc starts.
- `OpenStream` runs the validated argument vector attached without a terminal,
  preserving separate stdout/stderr bytes, real stdin half-close semantics,
  and process exit status. `OpenStreamConfigured` provides the same narrow
  pre-start process-configuration hook.
- Closing the PTY ends only the exec process and never signals the sandbox.

# Work Guidance

- Keep the temporary socket node-local and remove it immediately after the
  descriptor transfer.

# Verification

- Rootless workload and development integration tests exercise the shared
  console against real runsc.

# Child DOX Index
