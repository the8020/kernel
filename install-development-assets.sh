#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=${1:?usage: install-development-assets.sh <instance-root>}
ASSETS_STAGE=$(mktemp -d)
trap 'rm -rf -- "$ASSETS_STAGE"' EXIT
cp -a "$SOURCE_ROOT/defaults/scripts/." "$ASSETS_STAGE/"
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
bash "$RUNTIME_SOURCE/stage-service-runtime.sh" "$SOURCE_ROOT" "$ASSETS_STAGE/runtime"
install -m 0444 "$RUNTIME_SOURCE/development/deno.json" "$ASSETS_STAGE/deno.json"
install -m 0444 "$RUNTIME_SOURCE/protocol/generated.ts" "$ASSETS_STAGE/runtime/protocol.ts"
bash "$SOURCE_ROOT/defaults/kernel-scripts/refresh-development-scripts.sh" "$INSTANCE_ROOT" "$ASSETS_STAGE"
