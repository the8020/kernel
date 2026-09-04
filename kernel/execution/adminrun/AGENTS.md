# Purpose

- Implement administrative `runtime eval` and `runtime run` by creating bounded kernel-managed artifacts and submitting ordinary jobs.

# Ownership

- Materialize eval modules, securely copy a local module tree with imports, create sandbox-visible entrypoint URLs, invoke the shared job manager, and return execution and artifact identity.
- Do not evaluate code in Go, invoke native Deno, bypass gVisor, resolve remote dependencies, or become a general source repository.

# Local Contracts

- Public API: `New`, `Manager.Eval`, `Run`, and result/option types.
- Artifacts remain beneath the restrictive configured root, reject symlinks/control directories, and enforce file-count and byte limits. Directories are sandbox-traversable and files sandbox-readable for the canonical non-root image user; the host ancestors remain private and the sandbox bind is read-only.
- Administrative Workers receive read permission for only their generated artifact directory by default; explicitly requested broader permissions remain subject to the ordinary profile boundary.
- Eval wraps the submitted module’s default export in the ordinary job contract; run preserves relative local imports by copying the bounded source directory.
- Optional development workspaces are forwarded unchanged to the ordinary job profile/mount policy and never mounted directly by administrative execution.
- Completed non-reusable jobs release their transient sandbox before returning.
  Administrative execution therefore never performs a racy post-completion
  metrics read; live sandbox resources remain available through sandbox
  inspection while the sandbox exists.

# Work Guidance

- Use the same jobs/coordinator/supervisor/Worker path as all other programs and retain detached artifacts until explicit runtime cleanup.

# Verification

- Unit tests cover eval wrapper/result delegation, owner-scoped default artifact permissions, synchronous completion, relative-import copying, path/symlink/control-directory rejection, size limits, detached forwarding, and no host execution.

# Child DOX Index
