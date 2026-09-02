#!/usr/bin/env bash
set -euo pipefail

readonly INSTANCE_ROOT=/8020
readonly KERNEL=/usr/local/bin/kernel
readonly ADMIN=/usr/local/bin/admin
readonly PORTABLE_SMOKE=/usr/local/lib/the8020/smoke-portable.sh
readonly BOOTSTRAP_MARKER="$INSTANCE_ROOT/node/kernel/container-bootstrap.complete"

if (( $# > 0 )); then
  if (( $# != 1 )) || [[ "$1" != "serve" ]]; then
    exec "$@"
  fi
fi

if [[ ! -f "$INSTANCE_ROOT/node/kernel/paths.toml" ]]; then
  echo "80|20 instance data is missing from $INSTANCE_ROOT; use a new named volume or the image's bundled instance" >&2
  exit 1
fi

bootstrap_required=false
bootstrap_username=""
bootstrap_password=""
if [[ ! -f "$BOOTSTRAP_MARKER" ]]; then
  bootstrap_required=true
  bootstrap_username=${THE8020_USERNAME-admin}
  bootstrap_password=${THE8020_PASSWORD-admin}
  if [[ -z "$bootstrap_username" ]]; then
    echo "THE8020_USERNAME must not be empty on the first container boot" >&2
    exit 1
  fi
  if [[ -z "$bootstrap_password" ]]; then
    echo "THE8020_PASSWORD must not be empty on the first container boot" >&2
    exit 1
  fi
fi

# Do not pass bootstrap inputs to the kernel or any sandbox process. They are
# consulted only by this entrypoint while completing the first boot.
unset THE8020_USERNAME THE8020_PASSWORD || true

"$PORTABLE_SMOKE" \
  "$INSTANCE_ROOT/node/kernel/bin/runsc" \
  "$INSTANCE_ROOT/node/kernel/runtime/images/rootless/rootfs" \
  "$INSTANCE_ROOT/node/kernel/runtime/images/rootless/image.json" \
  "$INSTANCE_ROOT/node/kernel/runtime/images/rootless/smoke.json" \
  "$INSTANCE_ROOT/node/kernel/runtime/tmp"

kernel_pid=""
stop_kernel() {
  if [[ -n "$kernel_pid" ]] && kill -0 "$kernel_pid" 2>/dev/null; then
    kill -TERM "$kernel_pid" 2>/dev/null || true
  fi
}
finish() {
  stop_kernel
  if [[ -n "$kernel_pid" ]]; then
    wait "$kernel_pid" 2>/dev/null || true
  fi
}
trap finish EXIT
trap stop_kernel INT TERM HUP

"$KERNEL" --root "$INSTANCE_ROOT" &
kernel_pid=$!

admin_ready=false
for _ in {1..300}; do
  if "$ADMIN" --root "$INSTANCE_ROOT" system status >/dev/null 2>&1; then
    admin_ready=true
    break
  fi
  if ! kill -0 "$kernel_pid" 2>/dev/null; then
    set +e
    wait "$kernel_pid"
    kernel_status=$?
    set -e
    kernel_pid=""
    trap - EXIT INT TERM HUP
    exit "$kernel_status"
  fi
  sleep 0.1
done
if [[ "$admin_ready" != true ]]; then
  echo "80|20 administrative socket did not become ready within 30 seconds" >&2
  exit 1
fi

if [[ "$bootstrap_required" == true ]]; then
  users_json=$("$ADMIN" --root "$INSTANCE_ROOT" --json auth bootstrap-admin list)
  users_json=${users_json//$'\n'/}
  users_json=${users_json//$'\r'/}
  users_json=${users_json//$'\t'/}
  users_json=${users_json// /}
  if [[ "$users_json" == *'"result":{"users":[]}'* ]]; then
    printf '%s\n' "$bootstrap_password" |
      "$ADMIN" --root "$INSTANCE_ROOT" auth bootstrap-admin add "$bootstrap_username" --password-stdin >/dev/null
    echo "created first-boot 80|20 administrator: $bootstrap_username" >&2
  else
    echo "existing 80|20 administrators found; first-boot account creation skipped" >&2
  fi
  unset bootstrap_password
  install -m 0600 /dev/null "$BOOTSTRAP_MARKER"
fi

kernel_status=0
while true; do
  set +e
  wait "$kernel_pid"
  kernel_status=$?
  set -e
  if ! kill -0 "$kernel_pid" 2>/dev/null; then
    break
  fi
done
kernel_pid=""
trap - EXIT INT TERM HUP
exit "$kernel_status"
