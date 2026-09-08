Parent DOX: [kernel DOX](../AGENTS.md).

# Purpose

- Own container-specific runtime assets and startup behavior.

# Ownership

- `rootfs/` is the complete Docker runtime payload, copied to `/` by both the
  kernel and deploy Dockerfiles. Its entrypoint starts the kernel, creates the
  first login user, and waits for the public login service.
- Dockerfiles own image assembly, release metadata, and build-cache cleanup. The
  generic installer does not produce deployment payloads.

# Local Contracts

- The kernel Dockerfile builds its local tagged checkout without build
  arguments. The tag supplies the compatible package release line through the
  ordinary installer. It never selects or checks out another kernel version.
- The deploy Dockerfile selects the newest kernel patch for its requested
  major.minor line before using the same installer and runtime payload folders.
- Copy semantic directories rather than enumerating executables or helper files.
- Invoke the portable smoke helper from the instance's installed runtime
  definitions. Keep all startup credentials in process memory and preserve
  existing login users. Parse `users.list` JSON with the already bundled Deno
  binary; check enabled and password flags on the same user independently of
  property order. Malformed responses stop bootstrap without creating an
  account.
- Initial-account bootstrap runs once per persistent instance volume. After
  successful creation or confirming an existing login user, write the private
  `node/docker/initial-user.done` marker. Later starts skip all user commands and
  their Deno JSON parser, including after users are deleted or disabled. Failed
  bootstrap leaves no marker and retries on the next start. The marker belongs
  to the container entrypoint; image building and Go never create it.
- Login readiness probes immediately and waits 100 milliseconds between failed
  attempts, keeping the five-minute deadline and bounded serial requests.
- Runtime containers need the documented unconfined outer seccomp profile for
  nested rootless gVisor. Docker builds use the existing enclosing build
  sandbox.

# Work Guidance

- Keep container packaging out of Go and generic installation workflows.

# Verification

- With Deno on PATH, `bash docker-entrypoint_test.sh` checks progress, HTTP 200
  readiness, failure diagnostics, the curl dependency, existing-user detection
  with reordered JSON properties, one-time bootstrap and restart bypass after
  user removal, retry after failed creation, and rejection of invalid user
  responses without a completion marker.
- Build both the version-selected deploy image and the local tagged-checkout
  kernel image; exercise first-user bootstrap on a fresh instance.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
