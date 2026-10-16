# 80|20 kernel

## Development updates

Run this one-liner as root **inside the existing container** to update to
`0.8.7`. It supports rootless development containers at `/8020` running kernel
`0.7.4` or newer:

```bash
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.8.7/docker/update.sh -o /tmp/update.sh && bash /tmp/update.sh 0.8.7
```

After printing `Bootstrap started` or `Update started`, the updater continues in
the background. Follow its progress with:

```bash
tail -F /8020/node/kernel/update.log
```

Completion is reported as `update: completed 0.8.7`. Ctrl-C stops following the
log while the updater keeps running. Confirm the running version and startup
status with:

```bash
admin --root /8020 --json kernel.status
```

The update builds inside the container, requires at least **6 GiB free disk**,
and interrupts running work. It preserves package versions, data, and
configuration. Host access and container replacement are unnecessary.

Docker builds and updates share [install.sh](install.sh). The updater prepares a
separate staging instance and installs only platform assets. It requires a saved
script file for its background handoff, so direct `curl | sh` is unsupported.
