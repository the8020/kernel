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
  echo "usage: defaults/config/runtime/development/build-image.sh <source-root> [image-definition] [image-root] [versions-file] [work-root] [--rebuild]" >&2
  exit 2
fi
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "rootful development image build requires root" >&2
  exit 1
fi

BUILDKIT_BIN="$RUNTIME_ROOT/buildkit/bin"
if [[ ! -x "$BUILDKIT_BIN/buildctl" || ! -x "$BUILDKIT_BIN/buildkitd" ]]; then
  BUILDKIT_BIN=$(dirname "$(command -v buildctl || true)")
fi
if [[ ! -x "$BUILDKIT_BIN/buildctl" || ! -x "$BUILDKIT_BIN/buildkitd" ]]; then
  echo "buildctl and buildkitd are required for the rootful development image" >&2
  exit 1
fi
ROOTFS="$DEVELOPMENT_ROOT/rootfs"
RECORD="$DEVELOPMENT_ROOT/image.json"
TEMP_ROOT="$RUNTIME_ROOT/tmp"
CONTEXT_ROOT="$RUNTIME_ROOT/development-context"
mkdir -p "$DEVELOPMENT_ROOT" "$TEMP_ROOT" "$RUNTIME_ROOT/buildkit/state"

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
BASE_IMAGE=$(toml_value deno base_image)
CODEX_VERSION=$(toml_value codex version)
SOURCE_HASH="sha256:$({
  find "$RUNTIME_SOURCE/development/image/files" -type f -print0 | sort -z | xargs -0 sha256sum
  sha256sum "$IMAGE_DEFINITION/Containerfile" "$IMAGE_DEFINITION/build.sh" "$MANIFEST"
  printf '%s\n' "$BASE_MANIFEST" "$CODEX_VERSION" "$DENO_VERSION" "$(uname -m)"
} | sha256sum | awk '{print $1}')"
if [[ "$REBUILD" == false && -f "$RECORD" && -x "$ROOTFS/usr/bin/deno" ]] &&
  grep -Fq "\"source_hash\": \"$SOURCE_HASH\"" "$RECORD" &&
  grep -Fq '"materialization": "rootful-containerfile"' "$RECORD"; then
  echo "development image: unchanged input digest; reusing materialized image" >&2
  exit 0
fi

BUILDKIT_SOCKET="$RUNTIME_ROOT/buildkit/buildkitd.sock"
if ! "$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null 2>&1; then
  rm -f -- "$BUILDKIT_SOCKET"
  nohup "$BUILDKIT_BIN/buildkitd" --addr "unix://$BUILDKIT_SOCKET" --root "$RUNTIME_ROOT/buildkit/state" >"$RUNTIME_ROOT/buildkit/buildkitd.log" 2>&1 &
  BUILDKIT_PID=$!
  for _ in $(seq 1 200); do
    if "$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null 2>&1; then break; fi
    if ! kill -0 "$BUILDKIT_PID" 2>/dev/null; then
      wait "$BUILDKIT_PID" || true
      echo "BuildKit failed to start" >&2
      exit 1
    fi
    sleep 0.05
  done
fi
"$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null

rm -rf -- "$CONTEXT_ROOT"
install -d -m 0700 "$CONTEXT_ROOT/files"
cp -a "$RUNTIME_SOURCE/development/image/files/." "$CONTEXT_ROOT/files/"
install -m 0444 "$IMAGE_DEFINITION/build.sh" "$CONTEXT_ROOT/build.sh"

STAGE=$(mktemp -d "$TEMP_ROOT/development-containerfile-rootfs.XXXXXX")
trap 'rm -rf -- "${STAGE:-}"' EXIT
echo "development image [full 1/2]: building inside BuildKit" >&2
"$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" build \
  --frontend dockerfile.v0 \
  --local context="$CONTEXT_ROOT" \
  --local dockerfile="$IMAGE_DEFINITION" \
  --opt filename=Containerfile \
  --opt "build-arg:DENO_BASE=$BASE_IMAGE@$BASE_MANIFEST" \
  --opt "build-arg:CODEX_VERSION=$CODEX_VERSION" \
  --output "type=local,dest=$STAGE"

for required in deno codex git bash clear curl nano find grep sed apt-get apt-cache dpkg dpkg-deb; do
  if [[ ! -x "$STAGE/usr/bin/$required" && ! -x "$STAGE/usr/local/bin/$required" && ! -x "$STAGE/bin/$required" ]]; then
    echo "development Containerfile omitted $required" >&2
    exit 1
  fi
done
chroot "$STAGE" /usr/bin/apt-get --version >/dev/null
chroot "$STAGE" /usr/bin/dpkg-query -W apt >/dev/null
chroot "$STAGE" /usr/bin/env TERM=xterm /usr/bin/clear >/dev/null
chroot "$STAGE" /usr/bin/env TERM=xterm-256color /usr/bin/clear >/dev/null
if [[ "$(chroot "$STAGE" /usr/bin/deno --version | awk 'NR == 1 {print $2}')" != "$DENO_VERSION" ]]; then
  echo "development Containerfile has the wrong Deno version" >&2
  exit 1
fi
CODEX_OUTPUT=$(chroot "$STAGE" /usr/bin/env PATH=/usr/local/bin:/usr/bin:/bin codex --version)
if [[ "$CODEX_OUTPUT" != *"$CODEX_VERSION"* ]]; then
  echo "development Containerfile has the wrong Codex version" >&2
  exit 1
fi

rm -rf -- "$ROOTFS"
mv "$STAGE" "$ROOTFS"
STAGE=""
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "development image [full 2/2]: publishing verified image record" >&2
IMAGE_DIGEST="sha256:$(printf '%s\n%s\n' "$SOURCE_HASH" rootful-containerfile | sha256sum | awk '{print $1}')"
TEMP_RECORD="$RECORD.tmp"
printf '{\n  "schema": 1,\n  "image_digest": "%s",\n  "source_hash": "%s",\n  "materialization": "rootful-containerfile",\n  "built_at": "%s",\n  "codex_version": "%s",\n  "deno_version": "%s",\n  "build_status": "ready"\n}\n' \
  "$IMAGE_DIGEST" "$SOURCE_HASH" "$BUILT_AT" "$CODEX_VERSION" "$DENO_VERSION" > "$TEMP_RECORD"
chmod 0600 "$TEMP_RECORD"
mv -f "$TEMP_RECORD" "$RECORD"
trap - EXIT
