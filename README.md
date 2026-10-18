# 80|20 kernel

## Development updates

Run this one-liner as root **inside the existing container** to update to
`0.9.2`. It supports rootless development containers at `/8020` running kernel
`0.7.4` or newer:

```bash
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.9.2/docker/update.sh \
  -o /tmp/update.sh && bash /tmp/update.sh 0.9.2
```

After printing `Bootstrap started` or `Update started`, the updater continues in
the background. Follow its progress with:

```bash
tail -F /8020/node/kernel/update.log
```

Completion is reported as `update: completed 0.9.2`. Ctrl-C stops following the
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
