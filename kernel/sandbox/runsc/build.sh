#!/usr/bin/env bash
set -euo pipefail

OWNER=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
SOURCE_ROOT=$(cd "$OWNER/../../.." && pwd -P)
GO_CMD=${THE8020_BUILD_GO:-"$SOURCE_ROOT/.development/toolchains/go/bin/go"}
export GOWORK=off CGO_ENABLED=0
export GOCACHE=${GOCACHE:-"$SOURCE_ROOT/.development/cache/go-build"}
export GOMODCACHE=${GOMODCACHE:-"$SOURCE_ROOT/.development/cache/go-mod"}
LINK_FLAGS=
if [[ ${THE8020_OUTER_CONTAINER_BUILD:-false} == true ]]; then LINK_FLAGS=-s; fi
cd "$OWNER"
"$GO_CMD" mod download
"$GO_CMD" mod verify
SDK=$("$GO_CMD" list -m -f '{{.Dir}}' gvisor.dev/gvisor)
RELEASE=$(awk '/^\[gvisor\]/{section=1;next} /^\[/{section=0} section && $1=="release"{gsub(/"/,"",$3);print $3}' "$SOURCE_ROOT/defaults/config/runtime/versions.toml")
[[ "$RELEASE" == 20260817.0 ]] || { echo "Update the gVisor SDK with the pinned runtime release" >&2; exit 1; }
STAGE=$(mktemp -d)
trap 'rm -rf -- "$STAGE"' EXIT
# Isolate dependency corrections and leave the verified module cache intact.
cp -a "$SDK" "$STAGE/gvisor"
chmod -R u+w "$STAGE/gvisor"
(
  cd "$STAGE/gvisor"
  git apply "$OWNER/sdk.patch"
)
cp go.mod "$STAGE/go.mod"
cp go.sum "$STAGE/go.sum"
"$GO_CMD" mod edit -modfile="$STAGE/go.mod" -replace="gvisor.dev/gvisor=$STAGE/gvisor"
"$GO_CMD" test -mod=mod -modfile="$STAGE/go.mod" -trimpath ./...
mkdir -p "$SOURCE_ROOT/.development/runtime-bin"
"$GO_CMD" build -mod=mod -modfile="$STAGE/go.mod" -trimpath \
  -ldflags="$LINK_FLAGS -X 'gvisor.dev/gvisor/runsc/version.version=release-$RELEASE (the8020)'" \
  -o "$SOURCE_ROOT/.development/runtime-bin/runsc" .
# Both entrypoints execute the same engine, including its SDK fixes.
mkdir -p "$SOURCE_ROOT/.development/runtime-bin/gvisor-bin"
ln -sfn ../runsc "$SOURCE_ROOT/.development/runtime-bin/gvisor-bin/gvisor_sentry"
