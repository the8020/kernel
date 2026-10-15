#!/usr/bin/env bash
set -euo pipefail
SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -rf -- "$FIXTURE_ROOT"' EXIT
export THE8020_INSTANCE_ROOT="$FIXTURE_ROOT/instance"
KERNEL_ROOT="$THE8020_INSTANCE_ROOT/node/kernel"
for target in '0.8.4; bad' 0.8 1.02.3; do
  if bash "$SOURCE_ROOT/docker/update.sh" "$target" >/dev/null 2>&1; then exit 1; fi
done
[[ ! -e "$THE8020_INSTANCE_ROOT" ]]
if bash "$SOURCE_ROOT/docker/update.sh" --continue 0.8.4 "$FIXTURE_ROOT" >/dev/null 2>&1; then exit 1; fi
[[ -d "$FIXTURE_ROOT" ]]
mkdir -p "$KERNEL_ROOT/releases/0.8.3/scripts"
ln -s releases/0.8.3 "$KERNEL_ROOT/current"
ln -s current/scripts "$KERNEL_ROOT/scripts"
cat > "$KERNEL_ROOT/scripts/update.sh" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 1 && "$1" == 0.8.4 ]]
[[ -z ${THE8020_PASSWORD+x} && -z ${THE8020_USERNAME+x} ]]
echo 'managed updater called'
SCRIPT
export THE8020_USERNAME=private THE8020_PASSWORD=private
bash "$SOURCE_ROOT/docker/update.sh" 0.8.4 > "$FIXTURE_ROOT/output"
grep -Fq 'Update started' "$FIXTURE_ROOT/output"
for _ in {1..100}; do
  if grep -Fq 'managed updater called' "$KERNEL_ROOT/update.log"; then break; fi
  sleep 0.01
done
grep -Fq 'managed updater called' "$KERNEL_ROOT/update.log"
[[ $(stat -c '%a' "$KERNEL_ROOT/update.log") == 600 ]]
echo 'Inside-container updater checks passed'
