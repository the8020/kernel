#!/usr/bin/env bash
set -euo pipefail

readonly INSTANCE_ROOT=/8020
readonly KERNEL=/usr/local/bin/kernel
readonly ADMIN=/usr/local/bin/admin
readonly PORTABLE_SMOKE=/usr/local/lib/the8020/smoke-portable.sh

if (( $# > 0 )); then
  if (( $# != 1 )) || [[ "$1" != "serve" ]]; then
    exec "$@"
  fi
fi

if [[ ! -f "$INSTANCE_ROOT/kernel.toml" ]]; then
  echo "80|20 instance data is missing from $INSTANCE_ROOT; use a new named volume or the image's bundled instance" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required for container startup; rebuild using the current the8020/deploy Dockerfile" >&2
  exit 1
fi

initial_username=${THE8020_USERNAME:-admin}
initial_password=${THE8020_PASSWORD:-admin}

# Do not pass initial-user inputs to the kernel or any sandbox process. They are
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

echo "startup: starting the kernel" >&2
"$KERNEL" --root "$INSTANCE_ROOT" &
kernel_pid=$!

admin_ready=false
for _ in {1..300}; do
  if "$ADMIN" --root "$INSTANCE_ROOT" kernel.status >/dev/null 2>&1; then
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

echo "startup: waiting for package initialization and user commands" >&2
users_ready=false
users_json=""
for _ in {1..300}; do
  if users_json=$("$ADMIN" --root "$INSTANCE_ROOT" --json users.list 2>&1); then
    users_ready=true
    break
  fi
  if ! kill -0 "$kernel_pid" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if [[ "$users_ready" != true ]]; then
  echo "the8020/users commands did not become available within 30 seconds" >&2
  printf '%s\n' "$users_json" >&2
  "$ADMIN" --root "$INSTANCE_ROOT" kernel.status >&2 || true
  exit 1
fi
users_json=${users_json//$'\n'/}
users_json=${users_json//$'\r'/}
users_json=${users_json//$'\t'/}
users_json=${users_json// /}
if [[ "$users_json" != *'"enabled":true,"has_password":true'* ]]; then
  echo "startup: creating initial 80|20 user: $initial_username" >&2
  printf '%s\n' "$initial_password" |
    "$ADMIN" --root "$INSTANCE_ROOT" users.add "$initial_username" --password-stdin >/dev/null
  echo "created initial 80|20 user: $initial_username" >&2
else
  echo "initial user bootstrap skipped because an enabled login user already exists" >&2
fi
unset initial_username initial_password

echo "startup: waiting for the public login service" >&2
login_ready=false
login_response=""
login_deadline=$((SECONDS + 300))
while kill -0 "$kernel_pid" 2>/dev/null; do
  login_timeout=$((login_deadline - SECONDS))
  if (( login_timeout <= 0 )); then
    break
  fi
  if (( login_timeout > 30 )); then
    login_timeout=30
  fi
  if login_response=$(curl --silent --show-error --noproxy '*' \
      --connect-timeout 1 --max-time "$login_timeout" \
      --output /dev/null --write-out '%{http_code}' \
      "http://127.0.0.1:${THE8020_NETWORK_MAIN_PORT:-80}/the8020/uui/login/" 2>&1) &&
     [[ "$login_response" == 200 ]]; then
    login_ready=true
    break
  fi
  sleep 1
done
if [[ "$login_ready" != true ]]; then
  echo "80|20 startup failed: the public login service is unavailable" >&2
  printf '%s\n' "$login_response" >&2
  "$ADMIN" --root "$INSTANCE_ROOT" kernel.status >&2 || true
  exit 1
fi

echo "80|20 is ready" >&2

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
