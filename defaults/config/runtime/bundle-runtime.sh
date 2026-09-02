#!/usr/bin/env bash
set -euo pipefail

SOURCE=${1:-/opt/runtime/http-source}
DESTINATION=${2:-/opt/runtime/http}
if [[ ! -f "$SOURCE/mod.ts" || ! -f "$SOURCE/deno.json" || ! -f "$SOURCE/deno.lock" ]]; then
  echo "generic HTTP runtime source is incomplete: $SOURCE" >&2
  exit 1
fi

mkdir -p "$DESTINATION"
RUNTIME_ROOT=$(dirname "$DESTINATION")
if [[ ! -f "$RUNTIME_ROOT/deno.json" || ! -f "$RUNTIME_ROOT/deno.lock" ]]; then
  echo "runtime dependency configuration is incomplete: $RUNTIME_ROOT" >&2
  exit 1
fi
deno cache --config "$RUNTIME_ROOT/deno.json" \
  --lock "$RUNTIME_ROOT/deno.lock" --frozen npm:kysely@0.29.4
deno eval --config "$RUNTIME_ROOT/deno.json" --cached-only --check \
  'import type { ColumnType } from "kysely"; let value: ColumnType<string, string, string> | undefined; void value; await import("kysely")'
TEMPORARY="$DESTINATION/the8020_http.js.tmp"
deno bundle --config "$SOURCE/deno.json" --frozen --no-check "$SOURCE/mod.ts" --output "$TEMPORARY"
{
  printf '%s\n' '// @ts-self-types="./the8020_http.d.ts"'
  sed '/^\/\/ @ts-self-types=/d' "$TEMPORARY"
} > "$DESTINATION/the8020_http.js"
install -m 0444 "$SOURCE/the8020_http.d.ts" "$DESTINATION/the8020_http.d.ts"
rm -f -- "$TEMPORARY"
