# 80|20 kernel

## 0.9.17: password digests and development cleanup

Kernel **0.9.17** ships the single-call native password digest, shallow explicit
Deno workspace membership, and recoverable activation staging cleanup. Existing
0.x installations use the ordinary same-major update, then explicitly reset
legacy platform passwords through CBus. Existing Argon2 passwords are not
accepted and cannot be converted without plaintext. This accepted password
reset does not require a major release; the first version digit remains reserved
for actual breaking platform changes.

Use kernel **0.9.17**, users **0.9.8**, services **0.7.7**, UUI **0.9.14**,
dev-core **0.9.7** and dev-skills **0.9.4** together.
The ordered [bootstrap and recommended set](defaults/bootstrap-packages.toml)
pins all sixteen packages to existing 0.x lines. The other eleven packages keep
their previously recommended versions and commits. Package manifests and `/p/`
imports remain unversioned. This corrects the withdrawn 1.0.0 publication;
accepted implementation commits are preserved.

### Installation and rollout

For existing installations, follow [In-place development updates](#in-place-development-updates)
below, wait for kernel and recommended-package completion, then perform
[Password recovery](#password-recovery-and-key-dependence). No container
redeployment or cross-major transition is needed for these changes. The updater
restarts the kernel and interrupts running work while keeping its enclosing
launcher alive. The older bundled-Caddy conversion retains its separate
one-time container restart requirement described below.

Fresh installations build from the exact source tag using the ordinary
installer. For the existing kernel Docker build:

```bash
git clone --branch 0.9.17 https://github.com/the8020/kernel.git
cd kernel
docker build -t the8020:0.9.17 .
```

The Dockerfile derives release line `0.9` from the tag and installs the pinned
package set. The separate [deployment image](https://github.com/the8020/deploy)
can select that same line with `VERSION=0.9`. These commands build an image;
they do not upgrade an existing instance or constitute an installation test.

Select the instance and back up its database and original master seed separately
before updating. Keep that same seed: its loading, format, and existing signing
and encryption derivations are unchanged. The password digest adds a separate
purpose-derived key from the existing master; no key rotation or regeneration
is needed. Preserve private developer work and unresolved activation recovery.
Restore matching original keys with backups; replacing a key does not migrate
encrypted secrets. No password reset runs automatically.

Deleting the mistaken 1.0.0 tags does not remove copies already fetched by
consumers. An instance already installed as 1.0.0 cannot use the ordinary
updater to downgrade to 0.9.17; its installed version and cached package tags
need separate operator assessment. This publication does not modify any such
instance or remove its local tags.

Publish/install shared package owners before their consumers in the order in the
bootstrap list. Provision the matching kernel/runtime before executing the new
password callers. Retire old service Workers/streams with the existing hard
service restart or coordinated instance restart before the explicit reset;
never mix old/new password writers. All nodes of one system need the same
original master seed and matching versions. Restart development sandboxes after
the platform refresh so helper, SDK and generated-config mounts refresh; this
does not replace a retained sandbox's Deno binary. Fresh templates use Deno
2.9.8; updating a retained Deno binary is a separate operator action.

### Password recovery and key dependence

`crypto.password.digest` / `kernel.crypto.passwordDigest` accepts the password,
user identity and public nonce in one call and returns only the final digest.
Purpose-separated key derivation and HMAC-SHA-256 remain native. Trusted service
declarations or program package identity bind the credential namespace; users
owns constant-time comparison, account/session policy and its credential store.
Custom service stores and policies remain independent.

This accepts the database-only theft model, without a memory-hard work factor.
A compromised native key or usable digest oracle permits fast guessing. Back up
the **original master seed separately from the database**. Loss/replacement
requires password recovery and also affects tokens and encrypted secrets.

After the same-major update and recommended packages complete, use the selected
instance's native CBus,
which remains independent of platform password login. Inspect the recovery
administrator's roles with `auth.users.roles <existing-admin>`; repair an
assignment if necessary with
`auth.users.assign <existing-admin> the8020.users.administrator`. Then run:

```text
admin --root /absolute/selected-instance users.reset-passwords <existing-admin> --confirm
```

Enter and confirm the new password at the secure prompt; automation may use
`--password-stdin` from a secure pipe. Never place passwords in arguments, logs
or shell history. The transaction clears platform passwords and authentication
sessions, advances account versions, and enables the existing recovery account
with its new password. Roles, certificates and custom stores are preserved.
Sign in as that administrator and issue passwords through `users.set-password`.
Already-running work is not terminated by a password reset. A failed reset rolls
back; after a lost response inspect state and try the recovery login before
retrying, because another confirmed reset revokes newly issued credentials too.
See the [complete recovery procedure](https://github.com/the8020/users/blob/0.9.8/cbus/AGENTS.md).

### Workspace and conflict behavior

Standalone activate, SQL, log and UUI helpers bypass Deno config discovery with
unchanged grants. Ordinary checks and the language server retain explicit live
workspace membership from the merged shallow package roots, excluding hidden
conflict trees without deleting them.

Successful activation automatically retires verified generated staging and Git
registrations after recovery obligations finish. Confirmed rejected attempts
retire disposable generated copies, so retry/abort does not keep accumulating
them. Validation staging also follows durable completion and retry. Real
conflicts retain resolve/continue or safe-abort controls. Distinct user edits,
open/concurrently changed work, uncertain recovery and explicitly preserved
legacy data stay protected. There is no age-based directory purge.

### Verification boundary

This publication reuses the independently accepted source checks for the final
native digest and reset/recovery, namespace isolation, workspace membership and
helpers, and activation cleanup/retry. No additional release tests, fresh
installation, benchmarks or independent release review were run. Graphical
browser execution, installed/public-ingress/customer performance, full
containerd, live PostgreSQL and host-power-loss behavior remain unqualified.
Earlier host helper/Basic and rootless startup measurements are fixture results,
not deployment performance guarantees. No running instance was upgraded and no
live password reset or customer conflict cleanup was performed by publication.

## In-place development updates

Run it as root **inside the existing container** to update to
`0.9.17`. It supports rootless development containers at `/8020` running kernel
`0.7.4` or newer:

```bash
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.9.17/docker/update.sh \
  -o /tmp/update.sh && bash /tmp/update.sh 0.9.17
```

An instance already using the managed launcher can instead request
`admin --root /8020 kernel.update 0.9.17` through its ordinary native CBus.

After printing `Bootstrap started` or `Update started`, the updater continues in
the background. Follow its progress with:

```bash
tail -F /8020/node/kernel/update.log
```

Completion is reported as `update: completed 0.9.17`. Ctrl-C stops following the
log while the updater keeps running. Confirm the running version and startup
status with:

```bash
admin --root /8020 --json kernel.status
```

The update builds inside the container, requires at least **6 GiB free disk**,
and interrupts running work. It preserves data and configuration. After startup,
it installs missing recommended packages and advances older versions using the
new kernel's `node/kernel/scripts/bootstrap-packages.toml`. Entries with a
`version` select that Git tag; entries without one follow the remote default
branch. Updates only fast-forward and preserve local commits, uncommitted edits,
custom sources, and newer versions, reporting skipped packages. A package
installation failure keeps the update pending; retry the same kernel version
after repairing it. Unpublished edits in shared package directories can block
activation of other packages; publish that work or move it aside before
retrying.

To apply the current kernel's recommendations independently:

```bash
admin --root /8020 kernel.packages.recommended
```

For an installation using the former bundled Caddy proxy, this release also
updates the required HTTPS packages and installs the native HTTPS launcher. Wait
for `HTTPS update: prepared`, then restart the container once. That restart
stops Caddy, imports its certificates and settings, and transfers ports 443 and
9080 to the kernel. The existing certificate files remain available for
recovery. Local package edits or unsupported legacy settings stop preparation
for repair.

For dev/prod, update `/8020` first, then run the same script with
`THE8020_INSTANCE_ROOT=/8020-prod`. Wait for both updates before restarting the
container; production retains ports 8443 and 9081. Subsequent native kernel
updates keep the enclosing launcher running.

Docker builds and updates share [install.sh](install.sh). The updater prepares a
separate staging instance and installs only platform assets. It requires a saved
script file for its background handoff, so direct `curl | sh` is unsupported.
