#!/usr/bin/env bash
set -euo pipefail
SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -rf -- "$FIXTURE_ROOT"' EXIT
export FIXTURE_ROOT SOURCE_ROOT
mkdir "$FIXTURE_ROOT/tools"
cat > "$FIXTURE_ROOT/tools/git" <<'EOF'
#!/usr/bin/env bash
set -eu
if [[ "$1" == clone ]]; then
  destination="${!#}"
  mkdir -p "$destination/defaults" "$destination/docker/rootfs/usr/local/bin"
  cp -a "$SOURCE_ROOT/defaults/kernel-scripts" "$destination/defaults/"
  cp "$SOURCE_ROOT/docker/rootfs/usr/local/bin/docker-entrypoint.sh" "$destination/docker/rootfs/usr/local/bin/"
else printf '%040d\n' 0; fi
EOF
cat > "$FIXTURE_ROOT/tools/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$FIXTURE_ROOT/calls"
case "$1" in
  exec)
    if [[ "$2" == --detach ]]; then
      [[ -f "$FIXTURE_ROOT/restarted" ]]
      exit
    fi
    shift 2
    if [[ "$*" == 'test -L /8020/node/kernel/current' ]]; then
      [[ -f "$FIXTURE_ROOT/managed" ]]
    elif [[ "$*" == *'docker-entrypoint.sh.new /usr/local/bin/docker-entrypoint.sh' ]]; then
      touch "$FIXTURE_ROOT/entrypoint-installed"
    elif [[ "$*" == 'test -f /8020/node/kernel/run/launcher.pid' ]]; then
      [[ -f "$FIXTURE_ROOT/restarted" ]]
    fi
    ;;
  restart)
    [[ -f "$FIXTURE_ROOT/entrypoint-installed" ]]
    touch "$FIXTURE_ROOT/restarted"
    ;;
esac
EOF
chmod +x "$FIXTURE_ROOT/tools/"*
export PATH="$FIXTURE_ROOT/tools:$PATH"
bash "$SOURCE_ROOT/docker/update.sh" existing-container 0.8.3 > "$FIXTURE_ROOT/output"
grep -Fq 'restart -t 120 existing-container' "$FIXTURE_ROOT/calls"
grep -Fq 'exec --detach existing-container' "$FIXTURE_ROOT/calls"
grep -Fq 'update 0.8.3' "$FIXTURE_ROOT/calls"
grep -Fq 'Update started' "$FIXTURE_ROOT/output"
touch "$FIXTURE_ROOT/managed"
if bash "$SOURCE_ROOT/docker/update.sh" existing-container 0.8.3 > /dev/null 2>&1; then exit 1; fi
before=$(wc -l < "$FIXTURE_ROOT/calls")
if bash "$SOURCE_ROOT/docker/update.sh" existing-container '0.8.3; bad' > /dev/null 2>&1; then exit 1; fi
[[ $(wc -l < "$FIXTURE_ROOT/calls") == "$before" ]]
echo 'Legacy Docker updater checks passed'
