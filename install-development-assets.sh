#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=${1:?usage: install-development-assets.sh <instance-root>}
[[ -d "$INSTANCE_ROOT" ]] || { echo "instance root does not exist" >&2; exit 1; }
INSTANCE_ROOT=$(cd -- "$INSTANCE_ROOT" && pwd -P)

# Complete replacement removes retired assets and preserves source executability.
ASSETS_SOURCE="$SOURCE_ROOT/defaults/scripts"
ASSETS_ROOT="$INSTANCE_ROOT/scripts"
ASSETS_STAGE=$(mktemp -d "$INSTANCE_ROOT/.scripts-install.XXXXXX")
ASSETS_PREVIOUS=""
cleanup_assets_refresh() {
  if [[ -n "$ASSETS_STAGE" && -e "$ASSETS_STAGE" ]]; then
    rm -rf -- "$ASSETS_STAGE"
  fi
  if [[ -n "$ASSETS_PREVIOUS" && -e "$ASSETS_PREVIOUS" && ! -e "$ASSETS_ROOT" ]]; then
    mv -- "$ASSETS_PREVIOUS" "$ASSETS_ROOT"
  fi
}
trap cleanup_assets_refresh EXIT
chmod 0755 "$ASSETS_STAGE"
while IFS= read -r -d '' directory; do
  relative=${directory#"$ASSETS_SOURCE"/}
  [[ "$directory" == "$ASSETS_SOURCE" ]] && relative=""
  install -d -m 0755 "$ASSETS_STAGE/$relative"
done < <(find "$ASSETS_SOURCE" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$ASSETS_SOURCE"/}
  if source_mode=$(stat -c '%a' -- "$source" 2>/dev/null); then
    :
  elif source_mode=$(stat -f '%Lp' "$source" 2>/dev/null); then
    :
  else
    echo "cannot inspect development-asset mode: $source" >&2
    exit 1
  fi
  if [[ ! "$source_mode" =~ ^[0-7]{3,4}$ ]]; then
    echo "invalid development-asset mode $source_mode: $source" >&2
    exit 1
  fi
  mode=0444
  (( (8#$source_mode & 8#111) != 0 )) && mode=0555
  install -m "$mode" "$source" "$ASSETS_STAGE/$relative"
done < <(find "$ASSETS_SOURCE" -type f -print0)
if [[ -e "$ASSETS_ROOT" ]]; then
  ASSETS_PREVIOUS="$INSTANCE_ROOT/.scripts-previous.$$"
  if [[ -e "$ASSETS_PREVIOUS" ]]; then
    echo "asset backup path already exists: $ASSETS_PREVIOUS" >&2
    exit 1
  fi
  mv -- "$ASSETS_ROOT" "$ASSETS_PREVIOUS"
fi
mv -- "$ASSETS_STAGE" "$ASSETS_ROOT"
ASSETS_STAGE=""
if [[ -n "$ASSETS_PREVIOUS" ]]; then
  rm -rf -- "$ASSETS_PREVIOUS"
  ASSETS_PREVIOUS=""
fi
trap - EXIT
