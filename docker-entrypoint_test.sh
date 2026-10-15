#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
TEST_ROOT=$(mktemp -d)
entrypoint_pid=""
cleanup() {
  if [[ -n "$entrypoint_pid" ]]; then
    kill -TERM "$entrypoint_pid" 2>/dev/null || true
    wait "$entrypoint_pid" 2>/dev/null || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

mkdir -p "$TEST_ROOT/bin"
cat > "$TEST_ROOT/bin/kernel" <<'EOF'
#!/usr/bin/env bash
echo "$$" > "$CASE_ROOT/kernel.pid"
exec sleep 60
EOF
cat > "$TEST_ROOT/bin/admin" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == *users.list* ]]; then
  echo users.list >> "$CASE_ROOT/users-calls"
  cat "$CASE_ROOT/users.json"
elif [[ "$*" == *users.add* ]]; then
  echo users.add >> "$CASE_ROOT/users-calls"
  cat >/dev/null
  if [[ -f "$CASE_ROOT/fail-add" ]]; then
    echo 'initial user creation failed' >&2
    exit 1
  fi
  touch "$CASE_ROOT/user-created"
  printf '%s\n' '{"success":true,"result":{"users":[{"username":"admin","enabled":true,"has_password":true}]}}' > "$CASE_ROOT/users.json"
elif [[ "$*" == *auth.* ]]; then
  printf '%s\n' "$*" >> "$CASE_ROOT/auth-calls"
  if [[ "$*" == *auth.users.assign* && -f "$CASE_ROOT/fail-assign" ]]; then
    echo 'initial role assignment failed' >&2
    exit 1
  fi
else
  echo 'runtime_ready: false'
fi
EOF
cat > "$TEST_ROOT/bin/smoke" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cmp "$1" "$CASE_ROOT/runtime-bin/runsc"
[[ ! -e "$4" ]]
touch "$4"
EOF
cat > "$TEST_ROOT/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$CASE_ROOT/curl.args"
status=$(cat "$CASE_ROOT/http-status")
printf '%s\n' "$status" >> "$CASE_ROOT/probes"
if [[ "$CASE_MODE" == failed ]]; then
  kill -TERM "$(cat "$CASE_ROOT/kernel.pid")"
fi
printf '%s' "$status"
EOF
chmod +x "$TEST_ROOT/bin/"*
cp "$TEST_ROOT/bin/kernel" "$TEST_ROOT/bin/logd"
export PATH="$TEST_ROOT/bin:$PATH" THE8020_NETWORK_MAIN_PORT=18080

prepare() {
  export CASE_MODE=$1 CASE_ROOT="$TEST_ROOT/$1"
  export THE8020_INSTANCE_ROOT="$CASE_ROOT/instance"
  mkdir -p "$CASE_ROOT/instance"
  mkdir -p "$CASE_ROOT/runtime-bin" "$CASE_ROOT/instance/node/kernel/bin" \
    "$CASE_ROOT/instance/node/kernel/runtime/images/rootless" "$CASE_ROOT/instance/users/admin"
  printf 'new runtime\n' > "$CASE_ROOT/runtime-bin/runsc"
  chmod +x "$CASE_ROOT/runtime-bin/runsc"
  printf 'old runtime\n' > "$CASE_ROOT/instance/node/kernel/bin/runsc"
  printf 'private work\n' > "$CASE_ROOT/instance/users/admin/work.txt"
  touch "$CASE_ROOT/instance/node/kernel/runtime/images/rootless/smoke.json"
  mkdir -p "$CASE_ROOT/instance/node/kernel/runtime/definitions" \
    "$CASE_ROOT/runtime-state/definitions" "$CASE_ROOT/runtime-state/images/rootless"
  printf 'old definitions\n' > "$CASE_ROOT/instance/node/kernel/runtime/definitions/mod.ts"
  printf 'old image\n' > "$CASE_ROOT/instance/node/kernel/runtime/images/rootless/image.json"
  printf 'new definitions\n' > "$CASE_ROOT/runtime-state/definitions/mod.ts"
  printf 'new image\n' > "$CASE_ROOT/runtime-state/images/rootless/image.json"
  printf 'release-2\n' > "$CASE_ROOT/runtime-state/id"
  mkdir -p "$CASE_ROOT/runtime-state/kernel-scripts/update" "$CASE_ROOT/runtime-state/development-scripts"
  cp "$SOURCE_ROOT/defaults/kernel-scripts/"{launch.sh,update.sh,update.ts} "$CASE_ROOT/runtime-state/kernel-scripts/"
  cp "$TEST_ROOT/bin/smoke" "$CASE_ROOT/runtime-state/definitions/smoke-portable.sh"
  printf 'kernel_tag=0.8.0\nkernel_commit=%040d\n' 0 > "$CASE_ROOT/release"
  touch "$CASE_ROOT/instance/kernel.toml"
  while IFS= read -r line; do
    case "$line" in
      readonly\ KERNEL=*) printf 'readonly KERNEL=%q\n' "$TEST_ROOT/bin/kernel" ;;
      readonly\ DENO=*) printf 'readonly DENO=%q\n' "$(command -v deno)" ;;
      readonly\ ADMIN=*) printf 'readonly ADMIN=%q\n' "$TEST_ROOT/bin/admin" ;;
      readonly\ LOGD=*) printf 'readonly LOGD=%q\n' "$TEST_ROOT/bin/logd" ;;
      readonly\ RELEASE=*) printf 'readonly RELEASE=%q\n' "$CASE_ROOT/release" ;;
      readonly\ RUNTIME_BIN=*) printf 'readonly RUNTIME_BIN=%q\n' "$CASE_ROOT/runtime-bin" ;;
      readonly\ RUNTIME_STATE=*) printf 'readonly RUNTIME_STATE=%q\n' "$CASE_ROOT/runtime-state" ;;
      readonly\ PORTABLE_SMOKE=*) printf 'readonly PORTABLE_SMOKE=%q\n' "$TEST_ROOT/bin/smoke" ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < "$SOURCE_ROOT/docker/rootfs/usr/local/bin/docker-entrypoint.sh" > "$CASE_ROOT/entrypoint.sh"
  printf '%s\n' 503 > "$CASE_ROOT/http-status"
  printf '%s\n' '{"success":true,"result":{"users":[{"enabled":true,"full_name":"Admin","has_password":true}]}}' > "$CASE_ROOT/users.json"
}

wait_for() {
  for _ in {1..200}; do
    if grep -Fq -- "$1" "$2" 2>/dev/null; then
      return 0
    fi
    sleep 0.05
  done
  cat "$CASE_ROOT/output" >&2
  echo "did not observe: $1" >&2
  return 1
}

prepare ready
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for 503 "$CASE_ROOT/probes"
grep -Fq 'startup: waiting for the public login service' "$CASE_ROOT/output"
! grep -Fq '80|20 is ready' "$CASE_ROOT/output"
printf '%s\n' 303 > "$CASE_ROOT/http-status"
wait_for 303 "$CASE_ROOT/probes"
! grep -Fq '80|20 is ready' "$CASE_ROOT/output"
printf '%s\n' 200 > "$CASE_ROOT/http-status"
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ ! -f "$CASE_ROOT/user-created" ]]
[[ -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
[[ -L "$CASE_ROOT/instance/node/kernel/bin" ]]
grep -Fxq 'private work' "$CASE_ROOT/instance/users/admin/work.txt"
grep -Fq 'startup: installing the initial platform runtime from the image' "$CASE_ROOT/output"
grep -Fxq 'new definitions' "$CASE_ROOT/instance/node/kernel/runtime/definitions/mod.ts"
grep -Fxq 'new image' "$CASE_ROOT/instance/node/kernel/runtime/images/rootless/image.json"
[[ $(readlink "$CASE_ROOT/instance/node/kernel/current") == releases/0.8.0 ]]
grep -Fq 'initial user bootstrap skipped' "$CASE_ROOT/output"
grep -Fxq 'http://127.0.0.1:18080/the8020/uui/login/' "$CASE_ROOT/curl.args"
grep -Fxq -- '--noproxy' "$CASE_ROOT/curl.args"
[[ $(grep -Fc '80|20 is ready' "$CASE_ROOT/output") == 1 ]]
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

# A selected newer release survives restart even when the factory payload differs.
kernel_root="$CASE_ROOT/instance/node/kernel"
cp -a "$kernel_root/releases/0.8.0" "$kernel_root/releases/0.8.2"
ln -s releases/0.8.2 "$kernel_root/current.next"
mv -Tf "$kernel_root/current.next" "$kernel_root/current"
printf 'factory changed\n' > "$CASE_ROOT/runtime-state/definitions/mod.ts"
printf 'factory changed\n' > "$CASE_ROOT/runtime-state/images/rootless/image.json"
: > "$CASE_ROOT/output"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ $(readlink "$kernel_root/current") == releases/0.8.2 ]]
grep -Fxq 'new definitions' "$kernel_root/runtime/definitions/mod.ts"
grep -Fxq 'new image' "$kernel_root/runtime/images/rootless/image.json"
! grep -Fq 'installing the initial platform runtime' "$CASE_ROOT/output"
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare failed
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint succeeded without a running login service' >&2
  exit 1
fi
! grep -Fq '80|20 is ready' "$CASE_ROOT/output"
grep -Fq 'startup failed: the public login service is unavailable' "$CASE_ROOT/output"
grep -Fxq 503 "$CASE_ROOT/output"
grep -Fq 'runtime_ready: false' "$CASE_ROOT/output"

prepare no-login
printf '%s\n' '{"success":true,"result":{"users":[{"enabled":true,"has_password":false},{"enabled":false,"has_password":true}]}}' > "$CASE_ROOT/users.json"
printf '%s\n' 200 > "$CASE_ROOT/http-status"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ -f "$CASE_ROOT/user-created" ]]
[[ -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
grep -Fq 'auth.roles.create ** --if-missing' "$CASE_ROOT/auth-calls"
grep -Fq 'auth.roles.grant ** * *' "$CASE_ROOT/auth-calls"
grep -Fq 'auth.users.assign admin **' "$CASE_ROOT/auth-calls"
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.pending" ]]
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

# Restart the same initialized volume after all users have been removed.
# Bootstrap must issue no user command and must not recreate the default user.
printf '%s\n' '{"success":true,"result":{"users":[]}}' > "$CASE_ROOT/users.json"
rm "$CASE_ROOT/user-created" "$CASE_ROOT/users-calls"
: > "$CASE_ROOT/output"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ ! -f "$CASE_ROOT/users-calls" && ! -f "$CASE_ROOT/user-created" ]]
! grep -Fq 'installing the initial platform runtime' "$CASE_ROOT/output"
[[ -L "$CASE_ROOT/instance/node/kernel/bin" ]]
grep -Fxq 'private work' "$CASE_ROOT/instance/users/admin/work.txt"
! grep -Fq 'waiting for package initialization and user commands' "$CASE_ROOT/output"
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare failed-create
printf '%s\n' '{"success":true,"result":{"users":[]}}' > "$CASE_ROOT/users.json"
touch "$CASE_ROOT/fail-add"
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint accepted failed account creation' >&2
  exit 1
fi
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
rm "$CASE_ROOT/fail-add"
printf '%s\n' 200 > "$CASE_ROOT/http-status"
: > "$CASE_ROOT/output"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ -f "$CASE_ROOT/user-created" && -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare failed-assignment
printf '%s\n' '{"success":true,"result":{"users":[]}}' > "$CASE_ROOT/users.json"
touch "$CASE_ROOT/fail-assign"
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint accepted failed role assignment' >&2
  exit 1
fi
[[ -f "$CASE_ROOT/user-created" && -f "$CASE_ROOT/instance/node/docker/initial-user.pending" ]]
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
rm "$CASE_ROOT/fail-assign"
printf '%s\n' 200 > "$CASE_ROOT/http-status"
: > "$CASE_ROOT/output"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ $(grep -Fc users.add "$CASE_ROOT/users-calls") == 1 ]]
[[ $(grep -Fc auth.users.assign "$CASE_ROOT/auth-calls") == 2 ]]
[[ -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.pending" ]]
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare invalid-users
printf '%s\n' '{}' > "$CASE_ROOT/users.json"
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint accepted an invalid users.list response' >&2
  exit 1
fi
[[ ! -f "$CASE_ROOT/user-created" ]]
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]

prepare interrupted-seed
printf '%s\n' 200 > "$CASE_ROOT/http-status"
# An initial seed interrupted before publishing current is rebuilt from image.
runtime="$CASE_ROOT/instance/node/kernel/runtime"
rm -rf "$runtime/definitions"
mkdir -p "$CASE_ROOT/instance/node/kernel/releases/0.8.0"
printf 'incomplete seed\n' > "$CASE_ROOT/instance/node/kernel/releases/0.8.0/release.json"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
grep -Fxq 'new definitions' "$runtime/definitions/mod.ts"
grep -Fxq 'new image' "$runtime/images/rootless/image.json"
[[ $(readlink "$CASE_ROOT/instance/node/kernel/current") == releases/0.8.0 ]]
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare legacy-bootstrap
printf 'kernel_tag=0.7.4\nkernel_commit=9870f8363cf44b804df6234c3307b31ce0121f72\n' > "$CASE_ROOT/release"
printf '%s\n' 200 > "$CASE_ROOT/http-status"
bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1 &
entrypoint_pid=$!
wait_for '80|20 is ready' "$CASE_ROOT/output"
[[ $(readlink "$CASE_ROOT/instance/node/kernel/current") == releases/0.7.4 ]]
grep -Fxq 'private work' "$CASE_ROOT/instance/users/admin/work.txt"
kill -TERM "$entrypoint_pid"
wait "$entrypoint_pid" 2>/dev/null || true
entrypoint_pid=""

prepare missing-platform
printf '%s\n' 200 > "$CASE_ROOT/http-status"
rm -rf "$CASE_ROOT/runtime-state"
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint accepted missing updater payload' >&2
  exit 1
fi
grep -Fq 'kernel update scripts are missing' "$CASE_ROOT/output"
grep -Fxq 'old definitions' "$CASE_ROOT/instance/node/kernel/runtime/definitions/mod.ts"
[[ ! -e "$CASE_ROOT/instance/node/kernel/runtime/.image-runtime-state" ]]

prepare missing-curl
if PATH=/nonexistent "$BASH" "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint succeeded without its readiness dependency' >&2
  exit 1
fi
grep -Fq 'curl is required for container startup' "$CASE_ROOT/output"

echo 'Docker entrypoint checks passed: initial platform seeding, interrupted-seed recovery, retained instance runtime, fresh smoke, one-time account bootstrap, failed-bootstrap retries, HTTP readiness, and diagnostics.'
