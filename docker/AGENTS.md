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
  existing login users.
- Runtime containers need the documented unconfined outer seccomp profile for
  nested rootless gVisor. Docker builds use the existing enclosing build
  sandbox.

# Work Guidance

- Keep container packaging out of Go and generic installation workflows.

# Verification

- `bash docker-entrypoint_test.sh` checks progress, HTTP 200 readiness, failure
  diagnostics, and the curl dependency.
- Build both the version-selected deploy image and the local tagged-checkout
  kernel image; exercise first-user bootstrap on a fresh instance.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
