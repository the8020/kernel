#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
RUNTIME_ROOT=${2:-}
ROOTFS=${3:-}
shift 3 || true
if [[ -z "$SOURCE_ROOT" || -z "$RUNTIME_ROOT" || -z "$ROOTFS" || ! -x "$ROOTFS/usr/bin/env" || $# -eq 0 ]]; then
  echo "usage: runtime/run-rootfs-build.sh <source-root> <work-root> <rootfs> <command> [argument ...]" >&2
  exit 2
fi
SOURCE_ROOT=$(cd -- "$SOURCE_ROOT" && pwd -P)
RUNTIME_ROOT=$(cd -- "$RUNTIME_ROOT" && pwd -P)
ROOTFS=$(cd -- "$ROOTFS" && pwd -P)

case "$ROOTFS" in
  "$RUNTIME_ROOT/tmp/"*) ;;
  *) echo "image-build rootfs must be beneath $RUNTIME_ROOT/tmp" >&2; exit 2 ;;
esac

# A Docker/BuildKit RUN is already an isolated image-build execution. Avoid a
# nested runsc launch there because ordinary builders intentionally block the
# user-namespace re-exec that rootless gVisor requires. The final container
# performs the real runsc smoke before starting the kernel.
if [[ ${THE8020_OUTER_CONTAINER_BUILD:-false} == true ]]; then
  echo "image build: installing inside the outer container build sandbox" >&2
  chroot "$ROOTFS" /usr/bin/env \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/root \
    DEBIAN_FRONTEND=noninteractive \
    "$@"
  rm -rf -- "$ROOTFS/usr/share/doc"
  exit 0
fi

RUNSC="$RUNTIME_ROOT/gvisor/bin/runsc"
STAGE=$(mktemp -d "$RUNTIME_ROOT/tmp/image-build.XXXXXX")
ID="the8020-image-build-$$"
mkdir -p "$STAGE/root" "$STAGE/bundle" "$STAGE/output"
{
  printf '#!/bin/bash\nset -euo pipefail\n'
  printf '%q ' "$@"
  printf '\nprintf "image build: exporting isolated rootfs snapshot\\n" >&2\n'
  printf 'tar --numeric-owner -C / --exclude=./proc --exclude=./dev --exclude=./sys --exclude=./build-output --exclude=./usr/share/doc --exclude=./opt/runtime -czf /build-output/rootfs.tar.gz .\n'
  printf 'if [[ -d /opt/runtime ]]; then tar --numeric-owner --owner=0 --group=0 --mode=u+rwX,go+rX -C / -czf /build-output/runtime.tar.gz ./opt/runtime; fi\n'
} > "$STAGE/output/build.sh"
chmod 0555 "$STAGE/output/build.sh"
cat > "$STAGE/bundle/config.json" <<EOF
{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 0, "gid": 0},
    "args": ["/bin/bash", "/build-output/build.sh"],
    "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "DEBIAN_FRONTEND=noninteractive"],
    "cwd": "/",
    "capabilities": {
      "bounding": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_SETGID", "CAP_SETUID"],
      "effective": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_SETGID", "CAP_SETUID"],
      "inheritable": [],
      "permitted": ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_SETGID", "CAP_SETUID"],
      "ambient": []
    },
    "noNewPrivileges": false
  },
  "root": {"path": "$ROOTFS", "readonly": false},
  "hostname": "image-build",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "mode=755"]},
    {"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": ["nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"]},
    {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "nodev", "mode=1777"]},
    {"destination": "/build-output", "type": "bind", "source": "$STAGE/output", "options": ["rw"]}
  ],
  "linux": {"namespaces": [{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}]}
}
EOF
cleanup() {
  "$RUNSC" --root="$STAGE/root" --rootless=true --platform=systrap --network=host delete --force "$ID" >/dev/null 2>&1 || true
  rm -rf -- "$STAGE" "${SNAPSHOT:-}"
}
trap cleanup EXIT
"$RUNSC" --root="$STAGE/root" --rootless=true --platform=systrap --directfs=false --overlay2=root:self --network=host \
  --file-access=exclusive --file-access-mounts=shared run --bundle="$STAGE/bundle" "$ID"
[[ -s "$STAGE/output/rootfs.tar.gz" ]] || { echo "image build did not produce a rootfs" >&2; exit 1; }
SNAPSHOT=$(mktemp -d "$RUNTIME_ROOT/tmp/image-rootfs.XXXXXX")
echo "image build: importing rootfs snapshot" >&2
tar -xzf "$STAGE/output/rootfs.tar.gz" -C "$SNAPSHOT" --numeric-owner
if [[ -s "$STAGE/output/runtime.tar.gz" ]]; then
  tar -xzf "$STAGE/output/runtime.tar.gz" -C "$SNAPSHOT" --numeric-owner --same-permissions
fi
rm -rf -- "$ROOTFS"
mv "$SNAPSHOT" "$ROOTFS"
SNAPSHOT=""
