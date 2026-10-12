#!/usr/bin/env bash
set -euo pipefail

INSTANCE_ROOT=${1:?usage: launch.sh <instance-root>}
KERNEL_ROOT="$INSTANCE_ROOT/node/kernel"
export THE8020_INSTANCE_ROOT="$INSTANCE_ROOT"
mkdir -p "$KERNEL_ROOT/run"
printf '%s\n' "$$" > "$KERNEL_ROOT/run/launcher.pid"
kernel_pid=""
cleanup() {
  if [[ -n "$kernel_pid" ]]; then
    kill -TERM "$kernel_pid" 2>/dev/null || true
    wait "$kernel_pid" 2>/dev/null || true
  fi
  rm -f -- "$KERNEL_ROOT/run/launcher.pid"
}
trap cleanup EXIT
trap 'exit 143' TERM HUP
trap 'exit 130' INT

while true; do
  while [[ -f "$KERNEL_ROOT/update/request" || -f "$KERNEL_ROOT/update/offline" ]]; do
    if [[ -f "$KERNEL_ROOT/update/request" ]]; then
      result=0
      bash "$KERNEL_ROOT/scripts/update.sh" --apply >> "$KERNEL_ROOT/update.log" 2>&1 || result=$?
      if (( result != 0 && result != 75 )); then
        echo "kernel update paused; inspect $KERNEL_ROOT/update.log and retry the same version" >&2
      fi
    fi
    [[ -f "$KERNEL_ROOT/update/offline" || -f "$KERNEL_ROOT/update/request" ]] || break
    sleep 1
  done
  bash "$KERNEL_ROOT/runtime/definitions/smoke-portable.sh" \
    "$KERNEL_ROOT/bin/runsc" "$KERNEL_ROOT/runtime/images/rootless/rootfs" \
    "$KERNEL_ROOT/runtime/images/rootless/image.json" "$KERNEL_ROOT/runtime/images/rootless/smoke.json" \
    "$KERNEL_ROOT/runtime/tmp"
  "$KERNEL_ROOT/bin/kernel" --root "$INSTANCE_ROOT" &
  kernel_pid=$!
  if [[ -f "$KERNEL_ROOT/update/state.json" ]]; then
    for _ in {1..300}; do
      if "$KERNEL_ROOT/bin/admin" --root "$INSTANCE_ROOT" kernel.status >/dev/null 2>&1; then break; fi
      kill -0 "$kernel_pid" 2>/dev/null || break
      sleep 0.1
    done
    if ! bash "$KERNEL_ROOT/scripts/update.sh" --finish >> "$KERNEL_ROOT/update.log" 2>&1; then
      echo "kernel started; update verification is pending in $KERNEL_ROOT/update.log" >&2
    fi
  fi
  set +e
  wait "$kernel_pid"
  result=$?
  set -e
  kernel_pid=""
  if [[ ! -f "$KERNEL_ROOT/update/request" ]]; then exit "$result"; fi
  if (( result != 0 )); then
    echo "kernel did not shut down cleanly; update remains pending" >&2
    exit "$result"
  fi
done
