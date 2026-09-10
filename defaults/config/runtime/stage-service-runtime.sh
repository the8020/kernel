#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
DESTINATION=${2:-}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
if [[ -z "$SOURCE_ROOT" || -z "$DESTINATION" || ! -d "$RUNTIME_SOURCE/deno" ]]; then
  echo "usage: defaults/config/runtime/stage-service-runtime.sh <source-root> <destination|--sources>" >&2
  exit 2
fi

if [[ "$DESTINATION" == --sources ]]; then
  find "$RUNTIME_SOURCE/deno" \
    -type d \( -name examples -o -name test -o -name testdata -o -name node_modules \) -prune -o \
    -type f -name '*.ts' ! -name '*_test.ts' -print0 | sort -z
  exit 0
fi

while IFS= read -r -d '' source; do
  relative=${source#"$RUNTIME_SOURCE/deno/"}
  # HTTP source is bundled before publication; the other modules stay native.
  if [[ "$relative" == http/* ]]; then relative="http-source/${relative#http/}"; fi
  install -d -m 0755 "$(dirname "$DESTINATION/$relative")"
  install -m 0444 "$source" "$DESTINATION/$relative"
done < <("$BASH" "${BASH_SOURCE[0]}" "$SOURCE_ROOT" --sources)
install -m 0444 "$RUNTIME_SOURCE/deno/deno.json" "$DESTINATION/http-source/deno.json"
install -m 0444 "$RUNTIME_SOURCE/deno/deno.lock" "$DESTINATION/http-source/deno.lock"
