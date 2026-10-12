#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=${1:?usage: release-build.sh <installed-instance> <output-directory>}
OUTPUT=${2:?usage: release-build.sh <installed-instance> <output-directory>}
VERSION=$(git -C "$SOURCE_ROOT" describe --tags --exact-match HEAD)
[[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo "check out an exact kernel release tag" >&2; exit 1; }
[[ -z "$(git -C "$SOURCE_ROOT" status --porcelain --untracked-files=normal)" ]] || { echo "release source has uncommitted changes" >&2; exit 1; }
COMMIT=$(git -C "$SOURCE_ROOT" rev-parse HEAD)
case $(uname -m) in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported release architecture" >&2; exit 1 ;;
esac
[[ $(uname -s) == Linux ]] || { echo "kernel update archives require Linux" >&2; exit 1; }
[[ $("$INSTANCE_ROOT/node/kernel/bin/kernel" --version) == "$VERSION+$COMMIT" ]] || { echo "installed kernel does not match the release tag" >&2; exit 1; }
mkdir -p "$OUTPUT"
OUTPUT=$(cd -- "$OUTPUT" && pwd -P)
STAGE=$(mktemp -d)
trap 'rm -rf -- "$STAGE"' EXIT
mkdir -p "$STAGE/bin" "$STAGE/scripts" "$STAGE/development-scripts"
cp -a "$INSTANCE_ROOT/node/kernel/bin/." "$STAGE/bin/"
cp -a "$INSTANCE_ROOT/node/kernel/scripts/." "$STAGE/scripts/"
cp -a "$INSTANCE_ROOT/scripts/." "$STAGE/development-scripts/"
mkdir "$STAGE/runtime"
for directory in definitions images; do
  mkdir "$STAGE/runtime/$directory"
  cp -a "$INSTANCE_ROOT/node/kernel/runtime/$directory/." "$STAGE/runtime/$directory/"
done
find "$STAGE/runtime/images" -name smoke.json -delete
printf '{"version":"%s","commit":"%s","architecture":"%s"}\n' "$VERSION" "$COMMIT" "$ARCH" > "$STAGE/release.json"
FILENAME="the8020-kernel-$VERSION-linux-$ARCH.tar.gz"
tar --sort=name --mtime=@0 --numeric-owner -czf "$OUTPUT/$FILENAME" -C "$STAGE" release.json bin scripts runtime development-scripts
(cd -- "$OUTPUT" && sha256sum "$FILENAME" > "$FILENAME.sha256")
echo "release archive: $OUTPUT/$FILENAME"
