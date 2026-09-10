#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
GO_CMD=${THE8020_BUILD_GO:-"$SOURCE_ROOT/.development/toolchains/go/bin/go"}
export GOWORK=off
export GOCACHE=${GOCACHE:-"$SOURCE_ROOT/.development/cache/go-build"}
export GOMODCACHE=${GOMODCACHE:-"$SOURCE_ROOT/.development/cache/go-mod"}
LINK_FLAGS=
if [[ ${THE8020_OUTER_CONTAINER_BUILD:-false} == true ]]; then LINK_FLAGS=-s; fi
mkdir -p "$SOURCE_ROOT/.development/bin"
cd "$SOURCE_ROOT"
"$GO_CMD" run ./kernel/cbus/gen
(
  cd .development/generated
  "$GO_CMD" fmt ./...
  "$GO_CMD" build -mod=mod -trimpath -ldflags="$LINK_FLAGS" -o ../bin/kernel ./cmd/kernel
  "$GO_CMD" build -mod=mod -trimpath -ldflags="$LINK_FLAGS" -o ../bin/admin ./cmd/admin
)
"$GO_CMD" build -trimpath -ldflags="$LINK_FLAGS" -o .development/bin/logd ./kernel/logd
THE8020_BUILD_GO="$GO_CMD" "$SOURCE_ROOT/kernel/sandbox/runsc/build.sh"
