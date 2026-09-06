Parent DOX: [kernel/kernel DOX](../AGENTS.md).

# Purpose

- Own the standalone `logd` executable entrypoint.

# Ownership

- Convert inherited kernel descriptors into the logging daemon's control and raw
  input streams. Logging policy, producers, and status are kernel-controlled;
  ingestion and file ownership belong to `logging/daemon`.

# Local Contracts

- The kernel launches this binary with fd 3 as its private Unix control socket,
  fd 4 as raw kernel stdout input, and fd 5 as raw kernel stderr input. All
  three descriptors are required; no credentials or configuration enter
  arguments.
- SIGTERM/SIGINT request bounded drainage. Control EOF also drains after kernel
  death. The kernel starts logd in a separate process group and owns restart.
- Entrypoint errors use only bounded original stderr output, never recursive
  submission through the failed logging channel.

# Work Guidance

- Keep imports limited to standard library and the logging packages. Do not link
  the application kernel, generated command catalog, database, or runtime stack.

# Verification

- `go build ./kernel/logd` builds this independent executable; daemon tests own
  protocol, lifecycle, storage, and bounded-stream behavior.
- A native child-process test exercises the real entrypoint with inherited
  control/stdout/stderr descriptors and verifies final stack output after
  control EOF, bounded exit, UTF-8, and honest raw attribution.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
