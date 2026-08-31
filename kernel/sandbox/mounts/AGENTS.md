# Purpose

- Turn requested sandbox mounts into canonical policy-approved host/container paths.

# Ownership

- Own allowed host roots, symlink/traversal detection, protected-source/target rejection, mount metadata validation, and grouped-owner scope requirements.

# Local Contracts

- Public API: `Policy`, `NewPolicy`, and `Policy.Validate`.
- Host sources resolve beneath configured roots and never include host `/`,
  `/proc`, `/sys`, `/dev`, the containerd socket, or the instance's protected
  `node/kernel` directory.
- Relative sources resolve beneath the first configured root; a source is also
  rejected when mounting it would contain `node/kernel` or the containerd
  socket.
- Targets are canonical absolute paths under `/artifacts`, `/workspace`, `/runtime-cache`, or `/tmp`; `/opt/runtime` and kernel-controlled files cannot be overmounted.

# Lifecycle

- Validate after artifact/workspace creation and before profile hashing, state persistence, CNI, or container creation.

# Failure Behavior

- Missing paths, traversal, symlink escape, protected paths, invalid write/persistence policy, or owner-scope violations fail without host mutation.

# Concurrency

- Policies are immutable and safe for concurrent validation.

# Dependencies

- Go filesystem/path standard library and `sandbox/model`.

# Non-Responsibilities

- No OCI mount application, artifact copying, final distributed filesystem, or Worker permission decisions.

# Verification

- Unit tests cover read-only artifacts, writable workspaces, tmpfs, allowed
  roots, traversal, symlink escape, protected targets/sources, containerd
  socket, and `node/kernel` rejection.

# Child DOX Index
