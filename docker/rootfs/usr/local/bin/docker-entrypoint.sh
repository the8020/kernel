#!/usr/bin/env bash
set -euo pipefail

readonly INSTANCE_ROOT=/8020
readonly KERNEL=/usr/local/bin/kernel
readonly ADMIN=/usr/local/bin/admin
readonly RUNTIME_BIN=/usr/local/share/the8020/runtime-bin
readonly DENO="$INSTANCE_ROOT/node/kernel/runtime/images/rootless/rootfs/usr/bin/deno"
readonly PORTABLE_SMOKE="$INSTANCE_ROOT/node/kernel/runtime/definitions/smoke-portable.sh"
readonly BOOTSTRAP_DONE="$INSTANCE_ROOT/node/docker/initial-user.done"
readonly BOOTSTRAP_PENDING="$INSTANCE_ROOT/node/docker/initial-user.pending"

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
if [[ ! -x "$RUNTIME_BIN/runsc" ]]; then
  echo "the image's bundled sandbox runtime is missing" >&2
  exit 1
fi

# Runtime executables follow the image, including with an existing data volume.
rm -rf -- "$INSTANCE_ROOT/node/kernel/bin"
ln -s -- "$RUNTIME_BIN" "$INSTANCE_ROOT/node/kernel/bin"
# A retained smoke record may describe a different image's engine.
rm -f -- "$INSTANCE_ROOT/node/kernel/runtime/images/rootless/smoke.json"

initial_username=${THE8020_USERNAME:-admin}
initial_password=${THE8020_PASSWORD:-admin}

# Do not pass initial-user inputs to the kernel or any sandbox process. They are
# consulted only by this entrypoint while completing the first boot.
unset THE8020_USERNAME THE8020_PASSWORD || true

bash "$PORTABLE_SMOKE" \
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

if [[ ! -f "$BOOTSTRAP_DONE" ]]; then
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
  login_user_exists=$(printf '%s' "$users_json" | "$DENO" eval --quiet --no-config '
    const response = JSON.parse(await new Response(Deno.stdin.readable).text());
    if (response.success !== true || !Array.isArray(response.result?.users)) {
      throw new Error("users.list returned an invalid response");
    }
    console.log(response.result.users.some(user => user.enabled === true && user.has_password === true));
  ')
  if [[ "$login_user_exists" != true || -f "$BOOTSTRAP_PENDING" ]]; then
    resuming=false
    if [[ -f "$BOOTSTRAP_PENDING" ]]; then
      IFS= read -r initial_username < "$BOOTSTRAP_PENDING"
      resuming=true
    fi
    if [[ ! "$initial_username" =~ ^[a-z0-9]{3,32}$ ]]; then
      echo "initial username must contain 3-32 lowercase letters or digits" >&2
      exit 1
    fi
    initial_user_exists=$(printf '%s' "$users_json" | "$DENO" eval --quiet --no-config '
      const response = JSON.parse(await new Response(Deno.stdin.readable).text());
      console.log(response.result.users.some(user => user.username === Deno.args[0]));
    ' "$initial_username")
    if [[ "$resuming" != true && "$initial_user_exists" == true ]]; then
      echo "initial username already exists without an enabled login; choose another initial username" >&2
      exit 1
    fi
    (umask 077; mkdir -p "${BOOTSTRAP_PENDING%/*}"; printf '%s\n' "$initial_username" > "$BOOTSTRAP_PENDING")
    if [[ "$initial_user_exists" != true ]]; then
      echo "startup: creating initial 80|20 user: $initial_username" >&2
      printf '%s\n' "$initial_password" |
        "$ADMIN" --root "$INSTANCE_ROOT" users.add "$initial_username" --password-stdin >/dev/null
      echo "created initial 80|20 user: $initial_username" >&2
    fi
    echo "startup: assigning administrator permissions to $initial_username" >&2
    "$ADMIN" --root "$INSTANCE_ROOT" auth.roles.create '**' --if-missing >/dev/null
    "$ADMIN" --root "$INSTANCE_ROOT" auth.roles.grant '**' '*' '*' >/dev/null
    "$ADMIN" --root "$INSTANCE_ROOT" auth.users.assign "$initial_username" '**' >/dev/null
  else
    echo "initial user bootstrap skipped because an enabled login user already exists" >&2
  fi
  # Record only completed bootstrap; failed or interrupted creation retries.
  (umask 077; mkdir -p "${BOOTSTRAP_DONE%/*}"; touch "$BOOTSTRAP_DONE")
  rm -f -- "$BOOTSTRAP_PENDING"
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
  sleep 0.1
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
