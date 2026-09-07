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
