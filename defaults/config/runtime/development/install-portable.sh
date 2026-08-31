#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
IMAGE_DEFINITION=${2:-"$SOURCE_ROOT/config/runtime/development"}
DEVELOPMENT_ROOT=${3:-"$SOURCE_ROOT/node/kernel/runtime/images/development"}
MANIFEST=${4:-"$SOURCE_ROOT/config/runtime/versions.toml"}
RUNTIME_ROOT=${5:-"$SOURCE_ROOT/node/kernel/runtime"}
REBUILD=false
if [[ ${6:-} == --rebuild ]]; then REBUILD=true; fi
if [[ -z "$SOURCE_ROOT" || ! -f "$MANIFEST" || ! -f "$IMAGE_DEFINITION/Containerfile" || ! -f "$IMAGE_DEFINITION/build.sh" ]]; then
  echo "usage: defaults/config/runtime/development/install-portable.sh <source-root> [image-definition] [image-root] [versions-file] [work-root] [--rebuild]" >&2
  exit 2
fi

ROOTFS="$DEVELOPMENT_ROOT/rootfs"
RECORD="$DEVELOPMENT_ROOT/image.json"
TEMP_ROOT="$RUNTIME_ROOT/tmp"
mkdir -p "$DEVELOPMENT_ROOT" "$TEMP_ROOT"

toml_value() {
  local section=$1 key=$2
  awk -v wanted_section="[$section]" -v wanted_key="$key" '
    /^\[/ { section=$0 }
    section == wanted_section && $1 == wanted_key && $2 == "=" { value=$0; sub(/^[^=]+=[[:space:]]*/, "", value); gsub(/^"|"$/, "", value); print value; exit }
  ' "$MANIFEST"
}

case $(uname -m) in
  x86_64|amd64) BASE_MANIFEST=$(toml_value deno base_manifest_digest_amd64) ;;
  aarch64|arm64) BASE_MANIFEST=$(toml_value deno base_manifest_digest_arm64) ;;
  *) echo "unsupported development image architecture: $(uname -m)" >&2; exit 1 ;;
esac
DENO_VERSION=$(toml_value deno version)
CODEX_VERSION=$(toml_value codex version)
SOURCE_HASH="sha256:$({
  find "$RUNTIME_SOURCE/development/image/files" -type f -print0 | sort -z | xargs -0 sha256sum
  sha256sum "$IMAGE_DEFINITION/Containerfile" "$IMAGE_DEFINITION/build.sh" "$MANIFEST" "$RUNTIME_SOURCE/development/install-portable.sh" "$RUNTIME_SOURCE/materialize-oci-rootfs.sh" "$RUNTIME_SOURCE/run-rootfs-build.sh"
  printf '%s\n' "$BASE_MANIFEST" "$CODEX_VERSION" "$DENO_VERSION" "$(uname -m)"
} | sha256sum | awk '{print $1}')"
if [[ "$REBUILD" == false && -f "$RECORD" && -x "$ROOTFS/usr/bin/deno" ]] &&
  grep -Fq "\"source_hash\": \"$SOURCE_HASH\"" "$RECORD" &&
  grep -Fq '"materialization": "rootless-containerfile-equivalent"' "$RECORD"; then
  echo "development image: unchanged input digest; reusing materialized image" >&2
  exit 0
fi

STAGE=$(mktemp -d "$TEMP_ROOT/development-rootfs.XXXXXX")
trap 'rm -rf -- "${STAGE:-}"' EXIT
echo "development image [1/3]: materializing pinned OCI base" >&2
"$RUNTIME_SOURCE/materialize-oci-rootfs.sh" "$SOURCE_ROOT" "$RUNTIME_ROOT" "$BASE_MANIFEST" "$STAGE"
install -m 0555 "$IMAGE_DEFINITION/build.sh" "$STAGE/the8020-image-build.sh"
echo "development image [2/3]: installing declared packages inside gVisor" >&2
"$RUNTIME_SOURCE/run-rootfs-build.sh" "$SOURCE_ROOT" "$RUNTIME_ROOT" "$STAGE" /bin/sh -c \
  "/usr/bin/env CODEX_VERSION=$CODEX_VERSION /bin/bash /the8020-image-build.sh && rm -f /the8020-image-build.sh"

install -d -m 0755 "$STAGE/opt/development" "$STAGE/usr/local/bin"
cp -a "$RUNTIME_SOURCE/development/image/files/." "$STAGE/opt/development/"
chmod 0555 "$STAGE/opt/development/keepalive.sh"
install -m 0555 "$RUNTIME_SOURCE/development/image/files/activate" "$STAGE/usr/local/bin/activate"
install -m 0444 "$RUNTIME_SOURCE/development/image/files/profile" "$STAGE/etc/profile"

for required in deno codex git bash clear curl nano find grep sed apt-get apt-cache dpkg dpkg-deb; do
  if [[ ! -x "$STAGE/usr/bin/$required" && ! -x "$STAGE/usr/local/bin/$required" && ! -x "$STAGE/bin/$required" ]]; then
    echo "development image build omitted $required" >&2
    exit 1
  fi
done
"$RUNTIME_SOURCE/run-rootfs-build.sh" "$SOURCE_ROOT" "$RUNTIME_ROOT" "$STAGE" /bin/sh -c \
  "apt-get --version >/dev/null && dpkg-query -W apt >/dev/null && TERM=xterm clear >/dev/null && TERM=xterm-256color clear >/dev/null && test \"\$(deno --version | awk 'NR == 1 {print \$2}')\" = '$DENO_VERSION' && codex --version | grep -F '$CODEX_VERSION' >/dev/null"

rm -rf -- "$ROOTFS"
mv "$STAGE" "$ROOTFS"
STAGE=""
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "development image [3/3]: publishing verified image record" >&2
IMAGE_DIGEST="sha256:$(printf '%s\n%s\n' "$SOURCE_HASH" rootless-containerfile-equivalent-v1 | sha256sum | awk '{print $1}')"
TEMP_RECORD="$RECORD.tmp"
printf '{\n  "schema": 1,\n  "image_digest": "%s",\n  "source_hash": "%s",\n  "materialization": "rootless-containerfile-equivalent",\n  "built_at": "%s",\n  "codex_version": "%s",\n  "deno_version": "%s",\n  "build_status": "ready"\n}\n' \
  "$IMAGE_DIGEST" "$SOURCE_HASH" "$BUILT_AT" "$CODEX_VERSION" "$DENO_VERSION" > "$TEMP_RECORD"
chmod 0600 "$TEMP_RECORD"
mv -f "$TEMP_RECORD" "$RECORD"
trap - EXIT
