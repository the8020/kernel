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

For an existing `0.7.4` container, run this one-time bootstrap on the Docker
host. Replace `the8020` with its container name or ID:

```sh
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.8.3/docker/update.sh -o /tmp/the8020-update.sh
bash /tmp/the8020-update.sh the8020 0.8.3
docker exec the8020 tail -f /8020/node/kernel/update.log
```

The bootstrap installs the updater and restarts the same container once, then
starts the update in the background. It keeps the existing volume and packages.

Once the updater is installed, use the administrative command:

```sh
docker exec the8020 admin --root /8020 kernel.update 0.8.3
```

Or run the script directly, including after an offline migration failure:

```sh
docker exec the8020 /8020/node/kernel/scripts/update.sh 0.8.3
```

Use an exact same-major forward version. The updater fetches that Git tag and
prepares it locally with the existing installer before stopping the running
kernel. It requires network access and 6 GiB of free temporary disk space;
preparation may take several minutes. No new container or CI build is needed.
Packages keep their versions unless a migration explicitly updates them.

Updates interrupt live work. Repair a failed migration and retry the same
version; completed steps are retained. On an update-capable dev/prod image,
`/8020-prod` selects production, with its own installation and log. The one-time
bootstrap script targets the single-system `/8020` instance. Container
recreation requires an update-capable image to preserve the selected kernel.
