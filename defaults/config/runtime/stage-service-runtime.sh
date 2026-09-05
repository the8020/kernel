#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
DESTINATION=${2:-}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
if [[ -z "$SOURCE_ROOT" || -z "$DESTINATION" || ! -d "$RUNTIME_SOURCE/deno" ]]; then
  echo "usage: defaults/config/runtime/stage-service-runtime.sh <source-root> <destination>" >&2
  exit 2
fi

for directory in supervisor worker kernel context; do
  install -d -m 0755 "$DESTINATION/$directory"
  find "$RUNTIME_SOURCE/deno/$directory" -maxdepth 1 -type f \
    -name '*.ts' ! -name '*_test.ts' \
    -exec install -m 0444 '{}' "$DESTINATION/$directory/" \;
done
install -d -m 0755 "$DESTINATION/http-source"
install -m 0444 "$RUNTIME_SOURCE/deno/http/mod.ts" "$DESTINATION/http-source/mod.ts"
install -m 0444 "$RUNTIME_SOURCE/deno/http/the8020_http.d.ts" "$DESTINATION/http-source/the8020_http.d.ts"
install -m 0444 "$RUNTIME_SOURCE/deno/deno.json" "$DESTINATION/http-source/deno.json"
install -m 0444 "$RUNTIME_SOURCE/deno/deno.lock" "$DESTINATION/http-source/deno.lock"
