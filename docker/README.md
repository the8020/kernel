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

Run this **inside the existing container as root**, including on `0.7.4`:

```sh
curl -fsSL https://raw.githubusercontent.com/the8020/kernel/0.8.5/docker/update.sh -o /tmp/the8020-update.sh
bash /tmp/the8020-update.sh 0.8.5
tail -F /8020/node/kernel/update.log
```

The one-time bootstrap uses the existing kernel's graceful restart to adopt the
launcher while retaining the original child PID. The container stays running;
no Docker host access, Docker socket, container restart, or new image is needed.
The selected update then runs in the background. Existing packages stay in place.

Once installed, use the administrative command inside the container:

```sh
admin --root /8020 kernel.update 0.8.5
```

Or run the installed script directly, including after an offline migration failure:

```sh
/8020/node/kernel/scripts/update.sh 0.8.5
```

Use an exact same-major forward version. The updater fetches that Git tag and
prepares it locally with the existing installer before stopping the running
kernel. It requires network access and 6 GiB of free temporary disk space;
preparation may take several minutes. Packages keep their versions unless a
migration explicitly updates them. On older Debian images, the installer adds
the missing `bzip2` OS prerequisite automatically.

Updates interrupt live work. Repair a failed migration and retry the same
version; completed steps are retained. On an update-capable dev/prod image,
`THE8020_INSTANCE_ROOT=/8020-prod` selects production, with its own installation
and log. The one-time bootstrap targets single-system `0.7.4` or newer images.
Container recreation requires an update-capable image to preserve the selected
kernel; changes to the old container's entrypoint live in its writable layer.

The bootstrap and updater write progress and failures to
`node/kernel/update.log`. Bootstrap status probes have a five-second timeout
and a 90-second handoff deadline. Preparation streams installer output and
reports elapsed time every ten seconds. Inspect `kernel.status` for the running
build and application initialization errors; successful kernel replacement does
not mean application initialization succeeded. The update journal is
`node/kernel/update/state.json`.
