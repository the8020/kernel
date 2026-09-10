# Development workspace prototype

Parent: [analysis contract](AGENTS.md).

Ordinary `install.sh` and both Docker builds now include this implementation.
The installer calls `run.py build` and copies no test package snapshots. Python
3 is a build prerequisite. Published 0.6.2 builds used the legacy backend, which
restarted the development sandbox after activation; the separate prototype build
was the only path including the implementation below.

Use a fresh test instance when moving from 0.6.2 to this build: the activation
table's new `published` stage changes its database constraint. Keep the existing
instance/volume for its data and any private work; it is not migrated or deleted
by the build.

The installer puts one common `runsc` under `node/kernel/bin/` for services,
jobs and development. The kernel/admin/logd directory contains no second engine.
Docker keeps the binary payload outside its data volume and refreshes the node
link at startup, preserving existing data when the engine changes. Existing
legacy checkpoints containing private edits stop sandbox startup with recovery
instructions. Activate or export them using the previous kernel before
switching.

The prototype provides ordinary file edits over shared packages, file-level
copy-on-write, live untouched files, durable private Git state, and activation
without restarting the development sandbox. Ordinary package creation and
deletion use the same activation transaction as edits. File, directory, package,
and namespace renames record removals and additions without copying unchanged
assets; existing private edits move with their files. Package moves publish both
package IDs together. New folders need a valid `package.toml`, and activation
initializes missing Git metadata. Existing private history survives package
moves. References in source files to a renamed package ID remain explicit source
edits. Retained Git originals feed the same activation and conflict workflow.
[Rename check](rename-results.json).

The compiled native browser check uses two development sandboxes. A package
created after both start appears in the peer's view; private edits stay isolated
while untouched files follow the other developer's activations. Text/deletion
conflicts, UUI editing, terminal takeover, further shared updates during
resolution, continuation and whole-package deletion pass. A native private Git
commit made in that newly published package remains readable after shared
deletion and sandbox restart. The native database check also verifies catalog
retirement, retained table data and package recreation.
[Verification record](prototype-results.json).

Start the already built prototype for manual review:

```sh
cd /workspace/8020/kernel
python3 kernel/development/analysis/start-prototype.py
```

Open <http://127.0.0.1:8082/>. The first start creates the separate
`/tmp/8020-prototype-review` instance from the staged packages and prepared
runtime images. Later starts reuse its database, package sources and private
work. The source-workspace filesystem does not support the required socket
permissions, so runtime state uses native `/tmp`. This is a disposable instance;
host temporary-directory cleanup removes it.

The prepared review instance has a `reviewer` login; its generated password is
in `/tmp/8020-prototype-review/review-login.txt`. It also has a pending text and
deletion conflict in `the8020/conflict-demo`. Open **Development → Review
changes**, enter a commit message, then choose **Resolve conflicts**. Resolve
`label.txt` in the editor and either keep or delete `removed.txt`, then continue
activation. This example is ordinary package/private state and is not reseeded
on restart.

For a fresh instance without that login, create it once in a second terminal.
Run this command, then type the password and press Enter; password input stays
hidden:

```sh
/workspace/8020/kernel/.development/workflow-prototype/admin \
  --root /tmp/8020-prototype-review users.add reviewer --password-stdin
```

Log in as `reviewer` and open **Development**. The sandbox terminal and UUI
activation operate on this instance's package sources. SSH uses port 22222:

```sh
ssh -t -p 22222 reviewer@127.0.0.1 'the8020 terminal-id=1'
```

Stop it with the ordinary command, then use the same launch command to restart:

```sh
/workspace/8020/kernel/.development/workflow-prototype/admin \
  --root /tmp/8020-prototype-review kernel.shutdown
```

The launcher does not rebuild. If binary or staged package inputs need
rebuilding:

```sh
cd /workspace/8020/kernel
python3 kernel/development/analysis/run.py prototype
```

Outputs are in `.development/workflow-prototype/`: `kernel`, `admin`, `logd`,
`runsc`, and `package-workspace/`. On first setup, the launcher installs this
runsc under the review instance's `node/kernel/bin/` for both workload types.
The build uses the same tested compiler overlays as the installer and leaves
existing instances and the cached upstream SDK untouched. Staged package sources
omit environment files and host Git metadata. Rebuilding replaces these
generated inputs; it does not replace an existing review instance's packages. To
build alongside a running review instance:

```sh
WORKFLOW_PROTOTYPE_OUTPUT=/workspace/8020/kernel/.development/workflow-renames \
  python3 kernel/development/analysis/run.py prototype
```

That separate build does not upgrade the running review instance. The launcher
refuses to initialize over an existing directory whose first setup did not
finish. Keep the build directory and prepared runtime images in place for manual
use.

In a prototype sandbox, edit `/workspace/packages/<namespace>/<package>` with
ordinary tools, then run:

```sh
activate --preview
activate --message 'Describe the change'
```

Git support mounts share `/workspace/git/`: `private/` holds writable metadata,
`borrowed/` retains read-only objects, and `shared/` exposes the local upstream.
Shared package bookkeeping lives under `packages/.meta/`; its
`activation-locks/` files are created only for activation/source publication and
retain stable identities between operations. Sandbox startup skips per-package
Git initialization; ordinary Git access initializes its selected package without
creating lock files.

Use `--package namespace/package` to select one package; repeat the option for
several. A new package needs `package.toml` and its files; removing its
directory and activating publishes the deletion. Ordinary edits require no
private clone or checkout.

For a conflict, the helper prints the exact Git worktree and paths. Edit there,
use `git add` or `git rm`, commit the resolution, and rerun activation. `--json`
provides structured output; conflict exit status is 3.

In UUI, open **Development → Review changes → Activate all changes**. Conflicts
open the file selector and annotated editor. Save resolutions, select either
version, or delete a file; continue when Git has no unresolved files. An agent
can use the same worktree in the sandbox terminal at any point, followed by
**Refresh from Git** in UUI. Stale saves are rejected. Files over 48 KiB, binary
files, and symlinks use side selection/deletion or terminal Git. The UUI program
backend calls the sandbox Git helper through the ordinary kernel bridge; the
browser has no direct sandbox access.

Focused checks:

```sh
python3 kernel/development/analysis/run.py rename
python3 kernel/development/analysis/run.py activation
python3 kernel/development/analysis/run.py sparse
python3 kernel/development/analysis/run.py schema
```

The package-owned `dev-core/programs/development-test/prototype_browser.ts`
fixture exercises installer-built binaries through the existing native UUI
harness. Its `--runtime-root` must contain the newly built common runsc under
`node/kernel/bin/` alongside prepared runtime images; an older stock runsc
cannot run development mounts. From `/workspace/8020/uui`:

```sh
/workspace/8020/kernel/.development/runtime/development/rootfs/usr/bin/deno run -A --import-map=deno.local.json browser_e2e.ts \
  --source-root=/workspace/8020/kernel \
  --package-workspace=/workspace/8020/kernel/.development/workflow-prototype/package-workspace \
  --runtime-root=/workspace/8020/kernel/.development/named-terminal-test \
  --kernel=/workspace/8020/kernel/.development/bin/kernel \
  --admin=/workspace/8020/kernel/.development/bin/admin \
  --browser=/usr/bin/chromium \
  --fixture=../dev-core/programs/development-test/prototype_browser.ts
```

For the focused activation lifetime check, use the same invocation with
`--fixture=../dev-core/programs/development-test/activation_processes.ts`. It
checks process IDs/start times through both activation entry points and refuses
startup over a legacy checkpoint with private work. Use a relocated copy of the
complete executable directory to verify packaging.

Optimization, broader filesystem compatibility and host-power-loss qualification
remain deferred. Ordinary symlink deletion, rename and Git conflict side
selection pass the activation check, but reading an already open symlink after
unlink still fails the separate namespace check. That case remains deferred
compatibility work. Directory renames are covered by the focused rename check.
Existing benchmark records retain their original source hashes and timing
boundaries.

The browser exposed two lifecycle defects, now repaired in the prototype:
explicit reindex waits for selected busy packages and applies current state;
independent developer shutdowns overlap while retaining checkpoint-before-stop
order. Both owning regressions and race checks pass, and the rebuilt browser
workflow completes without the former diagnostics or remaining fixture
processes. The harness does not assert each kernel's exit status; full
graceful-shutdown qualification and shutdowns above eight concurrent developers
remain open.
