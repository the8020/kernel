#!/usr/bin/env bash
set -euo pipefail
unset THE8020_USERNAME THE8020_PASSWORD || true
ROOT=${THE8020_INSTANCE_ROOT:-/8020}
KERNEL_ROOT="$ROOT/node/kernel"

# Internal continuation: the original kernel has exec-replaced itself with the launcher.
if [[ ${1:-} == --continue ]]; then
  TARGET=${2:?missing target}; WORK=${3:?missing bootstrap directory}
  [[ "$WORK" == "$KERNEL_ROOT/update/bootstrap."* && -d "$WORK" ]] || exit 2
  for _ in {1..900}; do
    if [[ -L "$KERNEL_ROOT/current" && -f "$KERNEL_ROOT/run/launcher.pid" ]] &&
        "$KERNEL_ROOT/bin/admin" --root "$ROOT" kernel.status >/dev/null 2>&1; then
      rm -rf -- "$WORK"
      exec bash "$KERNEL_ROOT/scripts/update.sh" "$TARGET"
    fi
    sleep 0.1
  done
  echo 'The kernel did not adopt its launcher; inspect the kernel logs and retry.' >&2
  exit 1
fi

TARGET=${1:?usage: update.sh <major.minor.patch>}
[[ $# == 1 && "$TARGET" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Use an exact kernel version.' >&2; exit 2; }
[[ $EUID == 0 ]] || { echo 'Run this script as root inside the container.' >&2; exit 1; }
mkdir -p "$KERNEL_ROOT/update"
chmod 0700 "$KERNEL_ROOT/update"
touch "$KERNEL_ROOT/update.log"
chmod 0600 "$KERNEL_ROOT/update.log"
if [[ -L "$KERNEL_ROOT/current" ]]; then
  nohup bash "$KERNEL_ROOT/scripts/update.sh" "$TARGET" </dev/null >> "$KERNEL_ROOT/update.log" 2>&1 &
  printf 'Update started (PID %s). Follow %s/update.log\n' "$!" "$KERNEL_ROOT"
  exit
fi
ADMIN=$(command -v admin)
PREFIX=$(cd -- "$(dirname -- "$ADMIN")/.." && pwd -P)
EXECUTABLE="$PREFIX/bin/kernel"
ENTRYPOINT="$PREFIX/bin/docker-entrypoint.sh"
FACTORY="$PREFIX/share/the8020/runtime-state"
DENO="$KERNEL_ROOT/runtime/images/rootless/rootfs/usr/bin/deno"
exec 9> "$PREFIX/share/the8020/kernel-bootstrap.lock"
flock -n 9 || { echo 'A kernel bootstrap is already running.' >&2; exit 75; }
PID=$("$ADMIN" --root "$ROOT" --json kernel.status | "$DENO" eval --quiet --no-config 'console.log(JSON.parse(await new Response(Deno.stdin.readable).text()).result.pid)')
[[ "$PID" =~ ^[1-9][0-9]*$ ]] || { echo 'Cannot identify the active kernel.' >&2; exit 1; }
ACTIVE=$(readlink "/proc/$PID/exe")
[[ ${ACTIVE% (deleted)} == "$EXECUTABLE" ]] || { echo 'The active kernel is not the container kernel executable.' >&2; exit 1; }
FROM=$(sed -n 's/^kernel_tag=//p' "$PREFIX/share/the8020/release")
WORK=$(mktemp -d "$KERNEL_ROOT/update/bootstrap.XXXXXX")
REQUESTED=false
DRIVER=""
cleanup() {
  if [[ "$REQUESTED" != true ]]; then
    if [[ -n "$DRIVER" ]]; then kill "$DRIVER" 2>/dev/null || true; wait "$DRIVER" 2>/dev/null || true; fi
    if [[ -f "$WORK/kernel.native" ]]; then
      install -m 0555 "$WORK/kernel.native" "$EXECUTABLE.restore"
      mv -f -- "$EXECUTABLE.restore" "$EXECUTABLE"
    fi
    rm -rf -- "$WORK"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
git clone --depth 1 --single-branch --branch "$TARGET" https://github.com/the8020/kernel.git "$WORK/source"
[[ $(git -C "$WORK/source" rev-parse HEAD) == "$(git -C "$WORK/source" rev-parse "refs/tags/$TARGET^{commit}")" ]]
"$DENO" eval --quiet --no-config '
  const {compareVersions}=await import(Deno.args[0]);
  const [from,to]=Deno.args.slice(1);
  if (compareVersions(from,"0.7.4")<0 || from.split(".")[0]!==to.split(".")[0] || compareVersions(to,from)<=0) throw new Error("Use a forward same-major update from 0.7.4 or newer");
' "file://$WORK/source/defaults/kernel-scripts/update.ts" "$FROM" "$TARGET"
mkdir -p "$FACTORY/kernel-scripts" "$FACTORY/development-scripts"
cp -a "$WORK/source/defaults/kernel-scripts/." "$FACTORY/kernel-scripts/"
cp -a "$ROOT/scripts/." "$FACTORY/development-scripts/"
install -m 0755 "$WORK/source/docker/rootfs/usr/local/bin/docker-entrypoint.sh" "$ENTRYPOINT.new"
mv -f -- "$ENTRYPOINT.new" "$ENTRYPOINT"
install -m 0555 "/proc/$PID/exe" "$WORK/kernel.native"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'install -m 0555 %q %q\n' "$WORK/kernel.native" "$EXECUTABLE.restore"
  printf 'mv -f -- %q %q\n' "$EXECUTABLE.restore" "$EXECUTABLE"
  printf 'export THE8020_INSTANCE_ROOT=%q\n' "$ROOT"
  printf 'exec bash %q --adopt\n' "$ENTRYPOINT"
} > "$EXECUTABLE.new"
chmod 0755 "$EXECUTABLE.new"
# The running executable remains mapped; restart loads this one-time handoff.
mv -f -- "$EXECUTABLE.new" "$EXECUTABLE"
nohup bash "$WORK/source/docker/update.sh" --continue "$TARGET" "$WORK" </dev/null >> "$KERNEL_ROOT/update.log" 2>&1 &
DRIVER=$!
"$ADMIN" --root "$ROOT" kernel.restart >/dev/null
REQUESTED=true
printf 'Update started (PID %s). Follow %s/update.log\n' "$DRIVER" "$KERNEL_ROOT"
