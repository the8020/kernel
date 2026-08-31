#!/usr/bin/env bash
set -euo pipefail

SOURCE=${1:-/opt/runtime/http-source}
DESTINATION=${2:-/opt/runtime/http}
if [[ ! -f "$SOURCE/mod.ts" || ! -f "$SOURCE/deno.json" || ! -f "$SOURCE/deno.lock" ]]; then
  echo "generic HTTP runtime source is incomplete: $SOURCE" >&2
  exit 1
fi

mkdir -p "$DESTINATION"
TEMPORARY="$DESTINATION/the8020_http.js.tmp"
deno bundle --config "$SOURCE/deno.json" --frozen --no-check "$SOURCE/mod.ts" --output "$TEMPORARY"
{
  printf '%s\n' '// @ts-self-types="./the8020_http.d.ts"'
  sed '/^\/\/ @ts-self-types=/d' "$TEMPORARY"
} > "$DESTINATION/the8020_http.js"
install -m 0444 "$SOURCE/the8020_http.d.ts" "$DESTINATION/the8020_http.d.ts"
rm -f -- "$TEMPORARY"
