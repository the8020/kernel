#!/usr/bin/env bash
set -euo pipefail

CONTAINER=${1:?usage: update.sh <container> <major.minor.patch>}
KERNEL_VERSION=${2:?usage: update.sh <container> <major.minor.patch>}
[[ "$KERNEL_VERSION" =~ ^0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'use an exact 0.x kernel version' >&2; exit 2; }

if docker exec "$CONTAINER" test -L /8020/node/kernel/current; then
  echo 'This container already has an updater; use /8020/node/kernel/scripts/update.sh.' >&2
  exit 1
fi
docker exec "$CONTAINER" test -d /usr/local/share/the8020/runtime-state/images
STAGE=$(mktemp -d)
trap 'rm -rf -- "$STAGE"' EXIT
git clone --depth 1 --single-branch --branch "$KERNEL_VERSION" https://github.com/the8020/kernel.git "$STAGE/source"
[[ $(git -C "$STAGE/source" rev-parse HEAD) == "$(git -C "$STAGE/source" rev-parse "refs/tags/$KERNEL_VERSION^{commit}")" ]]
docker exec "$CONTAINER" mkdir -p /usr/local/share/the8020/runtime-state/kernel-scripts /usr/local/share/the8020/runtime-state/development-scripts
docker cp "$STAGE/source/defaults/kernel-scripts/." "$CONTAINER:/usr/local/share/the8020/runtime-state/kernel-scripts/"
docker exec "$CONTAINER" bash -c 'cp -a /8020/scripts/. /usr/local/share/the8020/runtime-state/development-scripts/'
docker cp "$STAGE/source/docker/rootfs/usr/local/bin/docker-entrypoint.sh" "$CONTAINER:/usr/local/bin/docker-entrypoint.sh.new"
docker exec "$CONTAINER" chmod 0755 /usr/local/bin/docker-entrypoint.sh.new
docker exec "$CONTAINER" mv /usr/local/bin/docker-entrypoint.sh.new /usr/local/bin/docker-entrypoint.sh
docker restart -t 120 "$CONTAINER"
ready=false
for _ in {1..300}; do
  if docker exec "$CONTAINER" test -f /8020/node/kernel/run/launcher.pid &&
      docker exec "$CONTAINER" admin --root /8020 kernel.status >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
[[ "$ready" == true ]] || { echo 'The instance did not restart; inspect docker logs.' >&2; exit 1; }
docker exec --detach "$CONTAINER" bash -c \
  'umask 077; exec bash /8020/node/kernel/scripts/update.sh "$1" >> /8020/node/kernel/update.log 2>&1' \
  update "$KERNEL_VERSION"
printf 'Update started. Follow progress with:\n  docker exec %q tail -f /8020/node/kernel/update.log\n' "$CONTAINER"
