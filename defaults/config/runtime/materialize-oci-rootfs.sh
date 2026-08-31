#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
RUNTIME_ROOT=${2:-}
MANIFEST_DIGEST=${3:-}
DESTINATION=${4:-}
if [[ -z "$SOURCE_ROOT" || -z "$RUNTIME_ROOT" || ! "$MANIFEST_DIGEST" =~ ^sha256:[0-9a-f]{64}$ || -z "$DESTINATION" ]]; then
  echo "usage: runtime/materialize-oci-rootfs.sh <source-root> <work-root> <manifest-digest> <destination>" >&2
  exit 2
fi

DOWNLOADS="$RUNTIME_ROOT/downloads/oci/denoland-deno"
case "$DESTINATION" in
  "$RUNTIME_ROOT/tmp/"*) ;;
  *) echo "OCI rootfs destination must be beneath $RUNTIME_ROOT/tmp" >&2; exit 2 ;;
esac
mkdir -p "$DOWNLOADS"

TOKEN_RESPONSE=$(curl --fail --silent --show-error --location \
  'https://auth.docker.io/token?service=registry.docker.io&scope=repository:denoland/deno:pull')
TOKEN=$(printf '%s' "$TOKEN_RESPONSE" | tr -d '\n' | sed -nE 's/.*"token"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p')
if [[ -z "$TOKEN" ]]; then
  echo "Docker registry did not return a bearer token" >&2
  exit 1
fi
MANIFEST="$DOWNLOADS/${MANIFEST_DIGEST#sha256:}.manifest.json"
curl --fail --silent --show-error --location \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  -o "$MANIFEST.tmp" \
  "https://registry-1.docker.io/v2/denoland/deno/manifests/$MANIFEST_DIGEST"
printf '%s  %s\n' "${MANIFEST_DIGEST#sha256:}" "$MANIFEST.tmp" | sha256sum --check --status
mv -f -- "$MANIFEST.tmp" "$MANIFEST"

mapfile -t LAYERS < <(
  tr '\n' ' ' < "$MANIFEST" |
    grep -oE '"mediaType"[[:space:]]*:[[:space:]]*"application/vnd\.oci\.image\.layer\.v1\.tar\+gzip"[^}]*' |
    grep -oE '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]{64}"' |
    sed -nE 's/.*"(sha256:[0-9a-f]{64})"/\1/p'
)
(( ${#LAYERS[@]} > 0 )) || { echo "pinned Deno OCI manifest has no supported layers" >&2; exit 1; }

rm -rf -- "$DESTINATION"
mkdir -p "$DESTINATION"
for digest in "${LAYERS[@]}"; do
  layer="$DOWNLOADS/${digest#sha256:}.tar.gz"
  if [[ ! -f "$layer" ]] || ! printf '%s  %s\n' "${digest#sha256:}" "$layer" | sha256sum --check --status; then
    curl --fail --silent --show-error --location \
      -H "Authorization: Bearer $TOKEN" \
      -o "$layer.tmp" \
      "https://registry-1.docker.io/v2/denoland/deno/blobs/$digest"
    printf '%s  %s\n' "${digest#sha256:}" "$layer.tmp" | sha256sum --check --status
    mv -f -- "$layer.tmp" "$layer"
  fi
  mapfile -t ENTRIES < <(tar -tzf "$layer")
  for entry in "${ENTRIES[@]}"; do
    [[ "$entry" != /* && "$entry" != ../* && "$entry" != */../* ]] || { echo "unsafe OCI layer path: $entry" >&2; exit 1; }
    name=${entry##*/}
    parent=${entry%/*}
    [[ "$parent" == "$entry" ]] && parent=.
    if [[ "$name" == .wh..wh..opq ]]; then
      if [[ -d "$DESTINATION/$parent" ]]; then find "$DESTINATION/$parent" -mindepth 1 -maxdepth 1 -exec rm -rf -- '{}' +; fi
    elif [[ "$name" == .wh.* ]]; then
      rm -rf -- "$DESTINATION/$parent/${name#.wh.}"
    fi
  done
  tar -xzf "$layer" -C "$DESTINATION" --exclude='*/.wh.*' --exclude='.wh.*' --no-same-owner
done
