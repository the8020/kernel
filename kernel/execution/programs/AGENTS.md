# Purpose

- Resolve and invoke ordinary programs from exact active package commits.

# Local Contracts

- A program is a package artifact; a job is one invocation of that artifact.
- Program invocations use one non-reusable ordinary job Worker.
- Arguments are spread into the program's default export. Secure inputs travel
  separately and are never included in job metadata.
- Kernel SDK command errors retain their structured code, message, and details
  through the ordinary job path; other program failures remain runtime errors.
- Resolution verifies the exact active package commit, manifest, containment,
  and real non-symlink entrypoint. Each invocation receives a short-lived exact
  package-content snapshot, without source-control metadata, mounted read-only
  at its canonical `/workspace/packages` path so package self-updates cannot
  change the running program.

# Verification

- Unit tests cover exact/stale resolution, argument and secure-input forwarding,
  output/error redaction, exact mounts, cleanup, and disabled Worker reuse.

# Child DOX Index
