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
else
  echo 'runtime_ready: false'
fi
EOF
cat > "$TEST_ROOT/bin/smoke" <<'EOF'
#!/usr/bin/env bash
exit 0
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
export PATH="$TEST_ROOT/bin:$PATH" THE8020_NETWORK_MAIN_PORT=18080

prepare() {
  export CASE_MODE=$1 CASE_ROOT="$TEST_ROOT/$1"
  mkdir -p "$CASE_ROOT/instance"
  touch "$CASE_ROOT/instance/kernel.toml"
  while IFS= read -r line; do
    case "$line" in
      readonly\ INSTANCE_ROOT=*) printf 'readonly INSTANCE_ROOT=%q\n' "$CASE_ROOT/instance" ;;
      readonly\ KERNEL=*) printf 'readonly KERNEL=%q\n' "$TEST_ROOT/bin/kernel" ;;
      readonly\ DENO=*) printf 'readonly DENO=%q\n' "$(command -v deno)" ;;
      readonly\ ADMIN=*) printf 'readonly ADMIN=%q\n' "$TEST_ROOT/bin/admin" ;;
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
grep -Fq 'initial user bootstrap skipped' "$CASE_ROOT/output"
grep -Fxq 'http://127.0.0.1:18080/the8020/uui/login/' "$CASE_ROOT/curl.args"
grep -Fxq -- '--noproxy' "$CASE_ROOT/curl.args"
[[ $(grep -Fc '80|20 is ready' "$CASE_ROOT/output") == 1 ]]
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

prepare invalid-users
printf '%s\n' '{}' > "$CASE_ROOT/users.json"
if bash "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint accepted an invalid users.list response' >&2
  exit 1
fi
[[ ! -f "$CASE_ROOT/user-created" ]]
[[ ! -f "$CASE_ROOT/instance/node/docker/initial-user.done" ]]

prepare missing-curl
if PATH=/nonexistent "$BASH" "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint succeeded without its readiness dependency' >&2
  exit 1
fi
grep -Fq 'curl is required for container startup' "$CASE_ROOT/output"

echo 'Docker entrypoint checks passed: one-time account bootstrap, restart bypass, failed-creation retry, HTTP readiness, diagnostics, and structural login-user detection.'
