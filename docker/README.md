# Build a checked-out kernel release

From the root of a Git checkout at a kernel release tag:

```sh
docker build --tag the8020 .
```

No build arguments are needed. The Dockerfile builds that checkout and derives
the compatible package release line from its Git tag. Keep `.git` available in
the local build context; environment files and local runtime data are excluded.

```sh
docker run --detach --name the8020 \
  --security-opt seccomp=unconfined \
  --publish 80:80 --publish 22:22 \
  --volume the8020-data:/8020 \
  the8020
```

Open <http://localhost/> and use `admin` / `admin` on a fresh volume. The
`THE8020_USERNAME` and `THE8020_PASSWORD` environment variables change only the
first-user defaults; existing users are preserved.

For remote version selection with `VERSION=<major.minor>`, use the
[deploy repository](https://github.com/the8020/deploy).

The newly created initial user receives role `**` with `"*" = "*"` through
ordinary auth commands. Interrupted first-user grants resume on restart.
Existing completed volumes retain their role assignments. Releases containing
this integration must also publish a compatible `the8020/auth` package tag.

## Update the kernel in the same container

On an update-capable instance, use the administrative command:

```sh
docker exec the8020 admin --root /8020 kernel.update 0.8.1
docker exec the8020 tail -f /8020/node/kernel/update.log
```

Choose an exact published kernel version with an update archive. Only
same-major forward updates are supported. Download and verification happen
before shutdown; live work is interrupted during installation. Packages retain
their versions unless a release migration explicitly updates them.

You can invoke the same updater directly, including after an offline migration
failure:

```sh
docker exec the8020 /8020/node/kernel/scripts/update.sh 0.8.1
```

Repair the reported failure and retry the same version. Completed migration
steps are retained. On the dev/prod image, `/8020-prod` selects production;
each instance has its own installation and log.

Existing containers at `0.8.0` need this one-time bootstrap from an updater-capable
kernel checkout, followed by a restart of the same container:

```sh
docker cp defaults/kernel-scripts the8020:/usr/local/share/the8020/runtime-state/kernel-scripts
docker exec the8020 sh -c 'cp -a /8020/scripts /usr/local/share/the8020/runtime-state/development-scripts'
docker cp docker/rootfs/usr/local/bin/docker-entrypoint.sh the8020:/usr/local/bin/docker-entrypoint.sh
docker restart the8020
```

The old kernel has no `kernel.update` command yet; invoke the installed script
directly for that first upgrade. Container recreation requires an update-capable
image to preserve the installed release. Older instance layouts and major
upgrades require separate migration handling.

Release maintainers build the ordinary tagged installation, then run
`./release-build.sh <instance-root> <output-directory>` and attach its archive
and `.sha256` file to the matching GitHub release.
