Parent DOX: [kernel/defaults DOX](../AGENTS.md).

# Purpose

- Provide platform-owned helpers installed into development sandboxes.

# Ownership

- Own `activate`, `activate.ts`, `activation-conflicts.ts`, `install-codex.sh`,
  and `install-claude.sh`; installation refreshes the instance scripts tree.
- `development-init.sh` is the mounted development entrypoint. It runs
  Bash key setup and `setup-agent-skills.sh` before the image's existing
  lock-directory/sleep init, including for retained system roots after an upgrade.

# Local Contracts

- Startup adds the Bash-only `CSI 13;2u` Shift+Enter binding to
  `/etc/bash.bashrc` once, using the default Ctrl+V/Ctrl+J quoted newline macro.
  Preserve existing system/user startup settings, ordinary Enter execution and
  raw key input to applications. New Bash shells pick it up after sandbox start;
  no image rebuild or durable-root reset is needed.

- `kernel-logs` delegates to dev-skills `8020-dev/scripts/kernel-logs.ts` with
  only the existing sandbox ingress environment and network permission. Package
  activation owns guidance/CLI behavior; installation stages this thin
  executable helper through the same read-only scripts mount as `sql`.

- Helpers are mounted read-only at `/workspace/scripts`; activation uses the
  typed kernel ingress and requires a publication message.
- `activate` prints readable status, conflicting paths, native Git commands and
  the retry command; `--json` retains structured output and `--help` gives
  usage.
- A resumed success says captured changes were activated and asks the developer
  to review remaining workspace changes; JSON preserves native recovery fields.
- `activate --preview --schema --package namespace/package` reads the shared
  metadata-only plan. Repeat `--force table-id.column` with explicit packages
  for exact incompatible-cell consent, without another prompt. Cleanup/native
  DDL run first. Only invalid cells in those columns may use legal NULL or an
  authored target default; valid cells, rows, keys and identities remain.
  Unsupported repairs stop without whole-column/table loss. Results report
  committed scope.
- The UUI program backend invokes `activation-conflicts.ts` through the ordinary
  kernel sandbox shell operation. The browser never accesses the sandbox
  directly. The helper reads bounded JSON on stdin, confines files to the
  retained Git worktree, exposes native index stages, rejects stale saves and
  unresolved markers, and stages edits/deletions with ordinary Git. It owns no
  second merge state. Text editing is limited to 48 KiB; larger/binary/linked
  files use native side selection or deletion. Link comparisons read target text
  from Git blobs; they never open the target file. Finishing refuses unresolved
  index entries and commits the resolution before the ordinary activation
  operation resumes publication.
- Agent installers are opt-in, target persistent sandbox root storage, and
  configure unattended full-access behavior. They also repeat discovery setup
  for the selected agent config directories before installing the CLI.
- Register `codex` and `claude` with `/usr/local/bin` symlinks to their native
  `~/.local/bin` entries. Both commands must work immediately in the invoking
  shell even when its PATH omits `~/.local/bin`; do not require sourcing a
  profile or restarting Bash. Verify by name without a temporary PATH override.
- `setup-agent-skills.sh` delegates to the activated package's read-only
  `/workspace/skills/builtin/setup-agent-skills.sh` through Bash. The package
  owns combining built-ins and `/workspace/skills/custom` into both agents'
  native discovery locations, preserving user content and adding instruction
  pointers. Developers may rerun this helper after changing skill names.
- Startup installs discovery links without an agent CLI, login, or model
  request. The package, not kernel installation, owns all skill content and
  merge policy.
- `uui` delegates to the independently shipped UUI-control skill script.
  Development startup attempts its native allowance exchange after discovery;
  unavailable authentication never prevents sandbox startup. The skill owns
  credential caching, expiry refresh, screen commands, and local session state.
- `sql` delegates to the independently shipped database skill's
  `the8020-dev-db/scripts/sql.ts`. The helper is executable on the existing
  development PATH; the package owns flags and output, while native ingress owns
  authentication and bounded SQL. It stores no credentials.

# Work Guidance

- Keep bootstrap inputs minimal and avoid changing unrelated user settings.

# Verification

- Kernel development unit tests exercise installers with isolated homes and
  upstream installer doubles, including immediate invocation in the same parent
  shell without `~/.local/bin` on PATH and repeated installation.
- Existing development and activation tests verify the shared activation path.
- `TestDevelopmentGuidance` verifies helper installation and mount confinement.
  The opt-in `TestRootlessDevelopmentGuidance` verifies the real dev-skills
  package, both discovery trees, publication, and persistent private skills.
  Discovery policy tests belong to the dev-skills package.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
