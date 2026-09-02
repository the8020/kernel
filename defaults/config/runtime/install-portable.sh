#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
IMAGE_ROOT=${2:-"$SOURCE_ROOT/node/kernel/runtime/images/rootless"}
MANIFEST=${3:-"$SOURCE_ROOT/config/runtime/versions.toml"}
CONTAINERFILE=${4:-"$SOURCE_ROOT/config/runtime/image/Containerfile"}
RUNTIME_DEFINITION=${5:-"$SOURCE_ROOT/config/runtime/image/deno.json"}
RUNTIME_LOCK=${RUNTIME_DEFINITION%.json}.lock
BUILD_SCRIPT="$(dirname "$CONTAINERFILE")/build.sh"
WORK_ROOT=${6:-"$SOURCE_ROOT/node/kernel/runtime"}
RUNSC_DESTINATION=${7:-"$SOURCE_ROOT/node/kernel/bin/runsc"}
PROTOCOL_SOURCE="$RUNTIME_SOURCE/protocol/generated.ts"
if [[ -z "$SOURCE_ROOT" || ! -f "$MANIFEST" || ! -f "$CONTAINERFILE" || ! -f "$BUILD_SCRIPT" || ! -f "$RUNTIME_DEFINITION" || ! -f "$RUNTIME_LOCK" || ! -f "$PROTOCOL_SOURCE" || -z "$WORK_ROOT" || -z "$RUNSC_DESTINATION" ]]; then
  echo "usage: defaults/config/runtime/install-portable.sh <source-root> [image-root] [versions-file] [Containerfile] [deno-config] [work-root] [runsc-destination]" >&2
  exit 2
fi

if [[ $(uname -s) != Linux ]]; then
  echo "portable gVisor runtime supports Linux only; leaving runtime unavailable on $(uname -s)" >&2
  exit 0
fi
if [[ "$SOURCE_ROOT" == *'"'* ]]; then
  echo "portable runtime does not support a project path containing a double quote" >&2
  exit 1
fi

RUNTIME_ROOT="$WORK_ROOT"
DOWNLOADS="$RUNTIME_ROOT/downloads"
TEMP_ROOT="$RUNTIME_ROOT/tmp"
GVISOR_ROOT="$RUNTIME_ROOT/gvisor/bin"
ROOTLESS_ROOT="$IMAGE_ROOT"
ROOTFS="$ROOTLESS_ROOT/rootfs"
RECORD="$ROOTLESS_ROOT/image.json"
SMOKE_RECORD="$ROOTLESS_ROOT/smoke.json"
mkdir -p "$DOWNLOADS" "$TEMP_ROOT" "$ROOTLESS_ROOT"

toml_value() {
  local section=$1 key=$2
  awk -v wanted_section="[$section]" -v wanted_key="$key" '
    /^\[/ { section=$0 }
    section == wanted_section && $1 == wanted_key && $2 == "=" {
      value=$0; sub(/^[^=]+=[[:space:]]*/, "", value); gsub(/^"|"$/, "", value); print value; exit
    }
  ' "$MANIFEST"
}

download() {
  local url=$1 destination=$2
  if [[ -f "$destination" ]]; then return; fi
  local partial="$destination.partial"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --retry 3 --output "$partial" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget --output-document="$partial" "$url"
  else
    echo "curl or wget is required to install the portable runtime" >&2
    exit 1
  fi
  mv -f -- "$partial" "$destination"
}

verify_sha512() { printf '%s  %s\n' "$1" "$2" | sha512sum --check --status; }

case $(uname -m) in
  x86_64|amd64)
    ARCHITECTURE=amd64
    GVISOR_ARCH=x86_64
    GVISOR_SHA=$(toml_value gvisor archive_sha512_amd64)
    RUNSC_SHA=$(toml_value gvisor runsc_sha512_amd64)
    BASE_MANIFEST=$(toml_value deno base_manifest_digest_amd64)
    ;;
  aarch64|arm64)
    ARCHITECTURE=arm64
    GVISOR_ARCH=aarch64
    GVISOR_SHA=$(toml_value gvisor archive_sha512_arm64)
    RUNSC_SHA=$(toml_value gvisor runsc_sha512_arm64)
    BASE_MANIFEST=$(toml_value deno base_manifest_digest_arm64)
    ;;
  *) echo "unsupported portable runtime architecture: $(uname -m)" >&2; exit 1 ;;
esac

GVISOR_RELEASE=$(toml_value gvisor release)
DENO_VERSION=$(toml_value deno version)
GVISOR_ARCHIVE="$DOWNLOADS/gvisor.tar.bz2"
download "https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$GVISOR_ARCH/gvisor.tar.bz2" "$GVISOR_ARCHIVE"
verify_sha512 "$GVISOR_SHA" "$GVISOR_ARCHIVE"

if [[ ! -x "$GVISOR_ROOT/runsc" ]] || ! verify_sha512 "$RUNSC_SHA" "$GVISOR_ROOT/runsc"; then
  GVISOR_STAGE=$(mktemp -d "$TEMP_ROOT/gvisor-portable.XXXXXX")
  trap 'rm -rf -- "${GVISOR_STAGE:-}" "${ROOTFS_STAGE:-}" "${SMOKE_STAGE:-}"' EXIT
  tar -xjf "$GVISOR_ARCHIVE" -C "$GVISOR_STAGE"
  verify_sha512 "$RUNSC_SHA" "$GVISOR_STAGE/runsc"
  rm -rf -- "$GVISOR_ROOT"
  install -d -m 0755 "$GVISOR_ROOT/gvisor-bin"
  install -m 0555 "$GVISOR_STAGE/runsc" "$GVISOR_ROOT/runsc"
  if [[ -d "$GVISOR_STAGE/gvisor-bin" ]]; then
    find "$GVISOR_STAGE/gvisor-bin" -maxdepth 1 -type f -exec install -m 0555 '{}' "$GVISOR_ROOT/gvisor-bin/" \;
  fi
  rm -rf -- "$GVISOR_STAGE"
  GVISOR_STAGE=""
fi
if [[ "$RUNSC_DESTINATION" != "$GVISOR_ROOT/runsc" ]]; then
  RUNSC_DESTINATION_ROOT=$(dirname "$RUNSC_DESTINATION")
  install -d -m 0700 "$RUNSC_DESTINATION_ROOT"
  install -m 0555 "$GVISOR_ROOT/runsc" "$RUNSC_DESTINATION"
  rm -rf -- "$RUNSC_DESTINATION_ROOT/gvisor-bin"
  if [[ -d "$GVISOR_ROOT/gvisor-bin" ]]; then
    install -d -m 0700 "$RUNSC_DESTINATION_ROOT/gvisor-bin"
    find "$GVISOR_ROOT/gvisor-bin" -maxdepth 1 -type f -exec install -m 0555 '{}' "$RUNSC_DESTINATION_ROOT/gvisor-bin/" \;
  fi
fi

SOURCE_INPUT=$(
  find "$RUNTIME_SOURCE/deno/supervisor" "$RUNTIME_SOURCE/deno/worker" "$RUNTIME_SOURCE/deno/kernel" "$RUNTIME_SOURCE/deno/http" -maxdepth 1 -type f \( -name '*.ts' -o -name '*.d.ts' \) ! -name '*_test.ts' -print0 | sort -z | xargs -0 sha256sum
  sha256sum "$RUNTIME_SOURCE/deno/deno.json" "$RUNTIME_SOURCE/deno/deno.lock" "$CONTAINERFILE" "$BUILD_SCRIPT" "$RUNTIME_DEFINITION" "$RUNTIME_LOCK" "$MANIFEST" "$RUNTIME_SOURCE/install-portable.sh" "$RUNTIME_SOURCE/materialize-oci-rootfs.sh" "$RUNTIME_SOURCE/run-rootfs-build.sh" "$RUNTIME_SOURCE/stage-service-runtime.sh" "$RUNTIME_SOURCE/bundle-runtime.sh" "$PROTOCOL_SOURCE" "$GVISOR_ROOT/runsc"
  printf '%s\n' "$BASE_MANIFEST" "$ARCHITECTURE"
)
SOURCE_HASH="sha256:$(printf '%s' "$SOURCE_INPUT" | sha256sum | awk '{print $1}')"
IMAGE_DIGEST="sha256:$(printf '%s\n%s\n' "$SOURCE_HASH" rootless-runtime-v3 | sha256sum | awk '{print $1}')"
SMOKE_RUNTIME=runsc-rootless-systrap
if [[ ${THE8020_OUTER_CONTAINER_BUILD:-false} == true ]]; then
  SMOKE_RUNTIME=outer-container-build
fi

if [[ -f "$RECORD" && -f "$SMOKE_RECORD" && -x "$ROOTFS/usr/bin/deno" ]] &&
   grep -Fq "\"source_hash\": \"$SOURCE_HASH\"" "$RECORD" &&
   grep -Fq "\"image_digest\": \"$IMAGE_DIGEST\"" "$SMOKE_RECORD" &&
   grep -Fq "\"runtime\": \"$SMOKE_RUNTIME\"" "$SMOKE_RECORD"; then
  echo "runtime image: unchanged input digest; reusing verified portable image" >&2
  exit 0
fi

ROOTFS_STAGE=$(mktemp -d "$TEMP_ROOT/rootfs-portable.XXXXXX")
trap 'rm -rf -- "${GVISOR_STAGE:-}" "${ROOTFS_STAGE:-}" "${SMOKE_STAGE:-}"' EXIT
echo "runtime image [1/4]: materializing pinned OCI base" >&2
"$RUNTIME_SOURCE/materialize-oci-rootfs.sh" "$SOURCE_ROOT" "$RUNTIME_ROOT" "$BASE_MANIFEST" "$ROOTFS_STAGE"
install -m 0555 "$BUILD_SCRIPT" "$ROOTFS_STAGE/the8020-image-build.sh"
install -d -m 0755 "$ROOTFS_STAGE/artifacts" "$ROOTFS_STAGE/runtime-cache" "$ROOTFS_STAGE/tmp/runtime"
"$RUNTIME_SOURCE/stage-service-runtime.sh" "$SOURCE_ROOT" "$ROOTFS_STAGE/opt/runtime"
install -m 0555 "$RUNTIME_SOURCE/bundle-runtime.sh" "$ROOTFS_STAGE/opt/runtime/bundle-runtime.sh"
install -m 0444 "$RUNTIME_DEFINITION" "$ROOTFS_STAGE/opt/runtime/deno.json"
install -m 0444 "$RUNTIME_LOCK" "$ROOTFS_STAGE/opt/runtime/deno.lock"
install -m 0444 "$PROTOCOL_SOURCE" "$ROOTFS_STAGE/opt/runtime/protocol.ts"
echo "runtime image [2/4]: installing declared packages and bundling generic modules" >&2
"$RUNTIME_SOURCE/run-rootfs-build.sh" "$SOURCE_ROOT" "$RUNTIME_ROOT" "$ROOTFS_STAGE" /bin/sh -c \
  '/bin/bash /the8020-image-build.sh && /bin/bash /opt/runtime/bundle-runtime.sh /opt/runtime/http-source /opt/runtime/http && rm -rf /the8020-image-build.sh /opt/runtime/bundle-runtime.sh /opt/runtime/http-source'

SMOKE_STAGE=""
if [[ "$SMOKE_RUNTIME" == outer-container-build ]]; then
  echo "runtime image [3/4]: smoke-testing bundled modules inside the outer container build sandbox" >&2
  chroot --userspec=1993:1993 "$ROOTFS_STAGE" /usr/bin/env \
    PATH=/usr/bin \
    HOME=/tmp \
    DENO_DIR=/tmp/deno-cache \
    DENO_NO_UPDATE_CHECK=1 \
    DENO_NO_PROMPT=1 \
    /usr/bin/deno eval --config=/opt/runtime/deno.json --cached-only \
    'await import("@the8020/http"); await import("@the8020/kernel"); await import("kysely"); console.log("the8020-outer-build-smoke")'
else
  SMOKE_STAGE=$(mktemp -d "$TEMP_ROOT/rootless-smoke.XXXXXX")
  echo "runtime image [3/4]: smoke-testing portable gVisor launch" >&2
  SMOKE_ID="the8020-rootless-smoke-$$"
  SMOKE_RUNSC_ROOT="$SMOKE_STAGE/runsc"
  SMOKE_OVERLAY="$SMOKE_STAGE/overlay"
  SMOKE_BUNDLE="$SMOKE_STAGE/bundle"
  mkdir -p "$SMOKE_RUNSC_ROOT" "$SMOKE_OVERLAY" "$SMOKE_BUNDLE"
  cat > "$SMOKE_BUNDLE/config.json" <<EOF
{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 1993, "gid": 1993},
    "args": ["/usr/bin/deno", "eval", "--config=/opt/runtime/deno.json", "--cached-only", "await import(\"@the8020/http\"); await import(\"@the8020/kernel\"); await import(\"kysely\"); console.log(\"the8020-rootless-smoke\")"],
    "env": ["PATH=/usr/bin", "HOME=/tmp", "DENO_DIR=/tmp/deno-cache", "DENO_NO_UPDATE_CHECK=1", "DENO_NO_PROMPT=1"],
    "cwd": "/tmp",
    "capabilities": {"bounding": [], "effective": [], "inheritable": [], "permitted": [], "ambient": []},
    "noNewPrivileges": true
  },
  "root": {"path": "$ROOTFS_STAGE", "readonly": false},
  "hostname": "rootless-smoke",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "mode=755"]},
    {"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": ["nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"]},
    {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "nodev", "mode=1777"]}
  ],
  "linux": {"namespaces": [{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}, {"type": "network"}]}
}
EOF

  SMOKE_OUTPUT="$SMOKE_STAGE/output.log"
  cleanup_smoke() {
    "$GVISOR_ROOT/runsc" --root="$SMOKE_RUNSC_ROOT" --rootless=true --platform=systrap --network=none --overlay2="root:dir=$SMOKE_OVERLAY" delete --force "$SMOKE_ID" >/dev/null 2>&1 || true
  }
  trap 'cleanup_smoke; rm -rf -- "${GVISOR_STAGE:-}" "${ROOTFS_STAGE:-}" "${SMOKE_STAGE:-}"' EXIT
  if ! "$GVISOR_ROOT/runsc" \
    --allow-rootfs-tar-annotation --root="$SMOKE_RUNSC_ROOT" --rootless=true --platform=systrap --directfs=false \
    --file-access=exclusive --file-access-mounts=shared --network=none --overlay2="root:dir=$SMOKE_OVERLAY" \
    --log="$SMOKE_STAGE/runsc.log" run --bundle="$SMOKE_BUNDLE" "$SMOKE_ID" >"$SMOKE_OUTPUT" 2>&1; then
    echo "portable rootless gVisor smoke test failed" >&2
    tail -80 "$SMOKE_OUTPUT" >&2 || true
    tail -80 "$SMOKE_STAGE/runsc.log" >&2 || true
    exit 1
  fi
  grep -Fq the8020-rootless-smoke "$SMOKE_OUTPUT"
  cleanup_smoke
fi

BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "runtime image [4/4]: publishing verified image records" >&2
PREVIOUS_ROOTFS="$ROOTLESS_ROOT/rootfs.previous.$$"
if [[ -e "$ROOTFS" ]]; then
  mv "$ROOTFS" "$PREVIOUS_ROOTFS"
fi
if ! mv "$ROOTFS_STAGE" "$ROOTFS"; then
  if [[ -e "$PREVIOUS_ROOTFS" ]]; then
    mv "$PREVIOUS_ROOTFS" "$ROOTFS"
  fi
  echo "failed to publish verified portable rootfs" >&2
  exit 1
fi
ROOTFS_STAGE=""
if [[ -e "$PREVIOUS_ROOTFS" ]]; then
  rm -rf -- "$PREVIOUS_ROOTFS"
fi
TEMP_RECORD="$ROOTLESS_ROOT/.image.json.tmp"
printf '{\n  "schema_version": 1,\n  "image_digest": "%s",\n  "deno_version": "%s",\n  "source_hash": "%s",\n  "runsc_release": "%s",\n  "built_at": "%s"\n}\n' \
  "$IMAGE_DIGEST" "$DENO_VERSION" "$SOURCE_HASH" "$GVISOR_RELEASE" "$BUILT_AT" > "$TEMP_RECORD"
chmod 0600 "$TEMP_RECORD"
mv -f -- "$TEMP_RECORD" "$RECORD"
TEMP_SMOKE="$ROOTLESS_ROOT/.smoke.json.tmp"
printf '{\n  "passed_at": "%s",\n  "image_digest": "%s",\n  "runtime": "%s"\n}\n' "$BUILT_AT" "$IMAGE_DIGEST" "$SMOKE_RUNTIME" > "$TEMP_SMOKE"
chmod 0600 "$TEMP_SMOKE"
mv -f -- "$TEMP_SMOKE" "$SMOKE_RECORD"
if [[ -n "$SMOKE_STAGE" ]]; then
  rm -rf -- "$SMOKE_STAGE"
  SMOKE_STAGE=""
fi
trap - EXIT
