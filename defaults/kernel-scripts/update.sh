#!/usr/bin/env bash
set -euo pipefail
unset THE8020_USERNAME THE8020_PASSWORD || true

SCRIPT_DIR=$(cd -L -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -L)
INSTANCE_ROOT=${THE8020_INSTANCE_ROOT:-$(cd -L -- "$SCRIPT_DIR/../../.." && pwd -P)}
DENO="$INSTANCE_ROOT/node/kernel/runtime/images/rootless/rootfs/usr/bin/deno"
exec "$DENO" run --quiet --no-config --allow-all "$SCRIPT_DIR/update.ts" "$INSTANCE_ROOT" "$@"
