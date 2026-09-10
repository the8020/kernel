Parent DOX: [development DOX](../AGENTS.md).

# Purpose

- Supply the native syscall client for workspace integration tests.

# Ownership

- `native_client.go` is built into disposable test sandboxes; production
  binaries never import or extract it.

# Local Contracts

- Exercise ordinary syscalls and Git paths through the real workspace mount.

# Work Guidance

# Verification

- The parent native workspace and activation tests build and invoke this client.
  `TestNativeSyscalls` compares the same rename, symlink, removed-directory,
  mmap, link, lock, and notification checks on host Linux and the workspace.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
