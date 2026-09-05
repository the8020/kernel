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
  echo '{"users":[{"enabled":true,"has_password":true}]}'
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
      readonly\ ADMIN=*) printf 'readonly ADMIN=%q\n' "$TEST_ROOT/bin/admin" ;;
      readonly\ PORTABLE_SMOKE=*) printf 'readonly PORTABLE_SMOKE=%q\n' "$TEST_ROOT/bin/smoke" ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < "$SOURCE_ROOT/docker-entrypoint.sh" > "$CASE_ROOT/entrypoint.sh"
  printf '%s\n' 503 > "$CASE_ROOT/http-status"
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

prepare missing-curl
if PATH=/nonexistent "$BASH" "$CASE_ROOT/entrypoint.sh" > "$CASE_ROOT/output" 2>&1; then
  echo 'entrypoint succeeded without its readiness dependency' >&2
  exit 1
fi
grep -Fq 'curl is required for container startup' "$CASE_ROOT/output"

echo 'Docker entrypoint checks passed: progress, HTTP 200 gating, failure diagnostics, and required curl.'
