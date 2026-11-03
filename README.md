# 80|20 kernel

## 0.10.2: order-independent recommended-package upgrades

Kernel **0.10.2** keeps the compatible **0.10.1** application tags and stages
all selected package candidates before evaluating schema, imports or hooks.
Mutually dependent packages such as db and i18n can upgrade together. The
bootstrap list controls reporting order; operators do not need to reorder it.
Dirty work, local commits, custom sources and newer versions remain protected.
A retained package must still be compatible: resolve the reported import/schema
failure before explicitly retrying. A failed fetch or preactivation validation
publishes no new package sources. Source switching retains candidates and backups for durable recovery;
committed schema/data receipts require forward recovery. Without such a receipt,
interrupted partial switches can restore the prior sources. This is not a
promise to roll back arbitrary migrations or hook side effects. Completed hook
phases and committed schema work are not repeated by recovery.

A completed **0.9.14** installation can select **0.10.2** through the ordinary
same-major updater. Its updater already invokes the new target kernel's
`kernel.packages.recommended`; no intermediate kernel or bootstrap-file edit is
required. Follow [In-place development updates](#in-place-development-updates),
back up the database and original master seed separately, retain operator
configuration and private work, and expect running work to be interrupted.
The legacy password recovery below still applies to 0.9.14 accounts.

### Recovering a pending 0.10.1 recommendation failure

The old updater deliberately rejects a different target while **0.10.1** is
pending. For the specific db/system/i18n failure, inspect the selected instance
before changing it:

```bash
instance=/absolute/selected-instance
admin --root "$instance" kernel.status
admin --root "$instance" kernel.packages.inspect the8020/db
admin --root "$instance" kernel.packages.inspect the8020/system
admin --root "$instance" kernel.packages.inspect the8020/i18n
```

This procedure requires kernel **0.10.1**, db already active at **0.10.1**,
system still at its previous version, and i18n absent or older than **0.10.1**.
Confirm the installed recommendation file selects the published first-party
**0.10.1** packages, the failed stage reports the missing i18n module, and there
is no unresolved committed-schema activation. Inspect
`node/kernel/update/state.json` read-only: target must be `0.10.1` and phase
`installed`. Back up the database, original master seed, node configuration and
private work before mutation. Check any existing i18n checkout with
`git -C "$instance/packages/the8020/i18n" status --short` and inspect its origin
and HEAD; retain customized sources, dirty work, local commits and newer
versions for separate assessment. Do not run this procedure against those.

Install the missing compatible peer using the native recovery commands. Restart
the kernel to let its existing activation recovery settle any failed schema
rollback, wait for runtime readiness and no pending database deployment, then
finish the original pending update:

```bash
admin --root "$instance" kernel.packages.set the8020 i18n   --source https://github.com/the8020/i18n.git --tag 0.10.1
admin --root "$instance" kernel.packages.synchronize --packages the8020/i18n
admin --root "$instance" kernel.restart
# Wait for kernel.status to show runtime_ready and no pending database deployment.
bash "$instance/node/kernel/scripts/update.sh" 0.10.1
```

Require successful command results, update phase `complete`, ready package
records matching the recommended tags, and HTTP 200 from `/health` and
`/the8020/uui/login/` on the configured listener. Then request **0.10.2** through
the ordinary updater. If recovery reports a different missing module, schema
conflict or unfinished activation, stop at that explicit error and retain its
sources/backups; this procedure covers the stated released package combination.
Do not delete worktrees or data, force versions, edit the update journal, or
replace a customized bootstrap file. If a fixed-kernel activation itself fails,
repair its reported cause and repeat `kernel.packages.recommended` or the same
pending kernel update; the coordinator resumes its retained attempt first.

## 0.10.1: 43 built-in languages

Release **0.10.1** recommends seventeen application packages with complete
catalogs for Tuxee’s 43 curated languages, including separate Portuguese and
Chinese variants. Fresh systems enable all 43; existing language settings stay
unchanged. The kernel runtime is unchanged from **0.10.0**.

Install the coordinated package set through the compatible
[bootstrap and recommended list](defaults/bootstrap-packages.toml). These
packages require kernel **0.10.0** or newer for invocation-local
`context.language`.

Package catalogs support customer database overrides and per-language fallback
chains. Languages administration sets the system default and a separate
login-screen default. Login remembers explicit browser choices; existing UUI
sessions retain their language. Service callers may override their account
default with the `the8020-language` header.

Fresh installations use
`git clone --branch 0.10.1
https://github.com/the8020/kernel.git` followed by
`docker build -t the8020:0.10.1 .`, or the deployment image with `VERSION=0.10`.
Existing 0.x systems use the ordinary same-major update and recommended-package
operation. The password recovery instructions below apply only when upgrading
from the legacy password format; this i18n release does not reset passwords.

## 0.9.18: reliable deployment startup

Kernel **0.9.18** waits for application initialization before creating the first
user, drains the kernel completely during failed-bootstrap shutdown, and fixes
rootless development sandbox startup inside rootless Docker. The recommended
package set now includes admin-core **0.9.1**.

The password and development changes introduced in 0.9.17 remain included:

The release ships the single-call native password digest, shallow explicit
Deno workspace membership, and recoverable activation staging cleanup. Existing
0.x installations use the ordinary same-major update, then explicitly reset
legacy platform passwords through CBus. Existing Argon2 passwords are not
accepted and cannot be converted without plaintext. This accepted password
reset does not require a major release; the first version digit remains reserved
for actual breaking platform changes.

Use kernel **0.9.18**, users **0.9.8**, services **0.7.7**, UUI **0.9.14**,
dev-core **0.9.7** and dev-skills **0.9.4** together.
The compatible [bootstrap and recommended set](defaults/bootstrap-packages.toml)
pins all sixteen packages to existing 0.x lines. The remaining packages keep
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
git clone --branch 0.9.18 https://github.com/the8020/kernel.git
cd kernel
docker build -t the8020:0.9.18 .
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
updater to downgrade to 0.9.18; its installed version and cached package tags
need separate operator assessment. This publication does not modify any such
instance or remove its local tags.

Apply the complete compatible recommended set together using kernel 0.10.2
or newer. Provision the matching kernel/runtime before executing the new
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

The release candidate passed fresh first-user bootstrap and native HTTPS
handoff, password/Basic login, graphical Home and Kernel calls, SSH development
with Deno 2.9.8, package activation, and the sandbox SQL helper in an isolated
container. Regression checks passed for readiness gating, failed-bootstrap
shutdown, and rootless UID/GID mapping; the development package tests and native
build also passed. Checks used bounded CPU, memory, and runtime.

Full containerd, live PostgreSQL, public ACME issuance, ARM hosts, and
host-power-loss behavior require separate qualification. No customer instance
is upgraded or reset by this publication.

## In-place development updates

Run it as root **inside the existing container** to update to
`0.10.1`. It supports rootless development containers at `/8020` running kernel
`0.7.4` or newer:

```bash
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.10.1/docker/update.sh \
  -o /tmp/update.sh && bash /tmp/update.sh 0.10.1
```

An instance already using the managed launcher can instead request
`admin --root /8020 kernel.update 0.10.1` through its ordinary native CBus.

After printing `Bootstrap started` or `Update started`, the updater continues in
the background. Follow its progress with:

```bash
tail -F /8020/node/kernel/update.log
```

Completion is reported as `update: completed 0.10.1`. Ctrl-C stops following the
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
