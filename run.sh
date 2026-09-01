#!/usr/bin/env bash
set -euo pipefail

restore_terminal() {
  if [[ -t 0 ]] && command -v stty >/dev/null 2>&1; then
    stty sane 2>/dev/null || true
  fi
}

# A previously interrupted raw-terminal client can leave line-feed translation
# and echo disabled in the parent shell. Repair that inherited state before the
# build emits anything, and again on every wrapper exit.
restore_terminal

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=$(pwd -P)

INSTALL_ARGUMENTS=("--instance-root=$INSTANCE_ROOT")
if [[ "${THE8020_SKIP_RUNTIME_HOST:-false}" == true ]]; then
  INSTALL_ARGUMENTS+=(--skip-runtime-host)
fi
INSTALL_ARGUMENTS+=(--skip-verification)
"$SOURCE_ROOT/install.sh" "${INSTALL_ARGUMENTS[@]}"

ADMIN="$SOURCE_ROOT/.development/bin/admin"
KERNEL="$SOURCE_ROOT/.development/bin/kernel"
KERNEL_PID=""
STARTED_KERNEL=false
SHUTDOWN_LAST_LINE=""
SHUTDOWN_TOTAL_STEPS=9

status_value() {
  local name=$1 status=$2
  sed -nE "s/^[[:space:]]*\"${name}\":[[:space:]]*(\"[^\"]*\"|true|false|[0-9]+),?$/\\1/p" <<<"$status" | sed 's/^"//; s/"$//'
}

kernel_process_running() {
  if [[ -z "$KERNEL_PID" ]] || ! kill -0 "$KERNEL_PID" 2>/dev/null; then
    return 1
  fi
  if [[ -r "/proc/$KERNEL_PID/status" ]]; then
    local process_state
    process_state=$(sed -nE 's/^State:[[:space:]]*([A-Z]).*/\1/p' "/proc/$KERNEL_PID/status")
    [[ -n "$process_state" && "$process_state" != Z ]]
    return
  fi
  return 0
}

show_shutdown_line() {
  local percent=$1 completed=$2 total=$3 message=$4 line
  line="Shutting down: ${percent}% (${completed}/${total}) ${message}"
  [[ "$line" != "$SHUTDOWN_LAST_LINE" ]] || return 0
  echo "$line" >&2
  SHUTDOWN_LAST_LINE=$line
}

show_shutdown_progress() {
  local status percent completed total message
  status=$("$ADMIN" --root "$INSTANCE_ROOT" --json system status 2>/dev/null) || return 1
  percent=$(sed -nE 's/^[[:space:]]*"shutdown_percent":[[:space:]]*([0-9]+),?$/\1/p' <<<"$status")
  completed=$(sed -nE 's/^[[:space:]]*"shutdown_completed_steps":[[:space:]]*([0-9]+),?$/\1/p' <<<"$status")
  total=$(sed -nE 's/^[[:space:]]*"shutdown_total_steps":[[:space:]]*([0-9]+),?$/\1/p' <<<"$status")
  message=$(sed -nE 's/^[[:space:]]*"shutdown_message":[[:space:]]*"([^"]*)",?$/\1/p' <<<"$status")
  if [[ -z "$percent" || -z "$completed" || -z "$total" || -z "$message" ]]; then
    return 1
  fi
  show_shutdown_line "$percent" "$completed" "$total" "$message"
}

finish_shutdown_progress() {
  local message=$1
  show_shutdown_line 100 "$SHUTDOWN_TOTAL_STEPS" "$SHUTDOWN_TOTAL_STEPS" "$message"
}

stop_started_kernel() {
  if [[ "$STARTED_KERNEL" == true && -n "$KERNEL_PID" ]]; then
    SHUTDOWN_LAST_LINE=""
    local graceful_requested=false forced=false
    if "$ADMIN" --root "$INSTANCE_ROOT" system shutdown >/dev/null 2>&1; then
      graceful_requested=true
      show_shutdown_line 0 0 "$SHUTDOWN_TOTAL_STEPS" "graceful shutdown requested"
      for _ in $(seq 1 120); do
        show_shutdown_progress || true
        kernel_process_running || break
        sleep 0.1
      done
    fi
    if kernel_process_running; then
      forced=true
      if [[ "$graceful_requested" == true ]]; then
        show_shutdown_line 0 0 "$SHUTDOWN_TOTAL_STEPS" "graceful deadline exceeded; sending termination signal"
      else
        show_shutdown_line 0 0 "$SHUTDOWN_TOTAL_STEPS" "administrative socket unavailable; sending termination signal"
      fi
      kill -TERM "$KERNEL_PID" 2>/dev/null || true
      for _ in $(seq 1 50); do
        show_shutdown_progress || true
        kernel_process_running || break
        sleep 0.1
      done
    fi
    if kernel_process_running; then
      forced=true
      kill -KILL "$KERNEL_PID" 2>/dev/null || true
    fi
    wait "$KERNEL_PID" 2>/dev/null || true
    if [[ "$forced" == true ]]; then
      finish_shutdown_progress "kernel stopped after forced termination"
    else
      finish_shutdown_progress "graceful shutdown complete"
    fi
    KERNEL_PID=""
  fi
}

report_start_failure() {
  local latest_log="" candidate
  for candidate in "$INSTANCE_ROOT"/node/kernel/logs/kernel-*.log; do
    [[ -f "$candidate" ]] || continue
    if [[ -z "$latest_log" || "$candidate" -nt "$latest_log" ]]; then
      latest_log=$candidate
    fi
  done
  if [[ -n "$latest_log" ]]; then
    echo "latest kernel log: $latest_log" >&2
    tail -n 20 "$latest_log" >&2 || true
  fi
}

trap 'stop_started_kernel; restore_terminal; exit 130' INT TERM HUP
trap 'stop_started_kernel; restore_terminal' EXIT

ATTACHED_STATUS=""
if ATTACHED_STATUS=$("$ADMIN" --root "$INSTANCE_ROOT" --json system status 2>/dev/null); then
  STARTED_KERNEL=false
  KERNEL_PID=$(status_value pid "$ATTACHED_STATUS")
  if [[ -z "$KERNEL_PID" ]]; then
    echo "running kernel status did not report its process ID" >&2
    exit 1
  fi
  echo "restarting attached kernel to activate the rebuilt binary" >&2
  "$ADMIN" --root "$INSTANCE_ROOT" system restart >/dev/null
  while true; do
    if ! kernel_process_running; then
      echo "attached kernel exited before completing its rebuild restart" >&2
      exit 1
    fi
    RESTARTED_STATUS=$("$ADMIN" --root "$INSTANCE_ROOT" --json system status 2>/dev/null || true)
    if [[ -n "$RESTARTED_STATUS" ]] &&
      [[ "$(status_value restart_requested "$RESTARTED_STATUS")" == false ]] &&
      [[ "$(status_value shutdown_requested "$RESTARTED_STATUS")" == false ]] &&
      [[ "$(status_value shutdown_step "$RESTARTED_STATUS")" == running ]]; then
      break
    fi
    sleep 0.1
  done
else
  "$KERNEL" --root "$INSTANCE_ROOT" &
  KERNEL_PID=$!
  STARTED_KERNEL=true

  READY=false
  READY_ATTEMPT=0
  while [[ "$READY" != true ]]; do
    READY_ATTEMPT=$((READY_ATTEMPT + 1))
    if "$ADMIN" --root "$INSTANCE_ROOT" system status >/dev/null 2>&1; then
      READY=true
      break
    fi
    if [[ -n "$KERNEL_PID" ]] && ! kill -0 "$KERNEL_PID" 2>/dev/null; then
      set +e
      wait "$KERNEL_PID"
      KERNEL_STATUS=$?
      set -e
      KERNEL_PID=""
      STARTED_KERNEL=false
      echo "kernel exited before its administrative socket became ready (status $KERNEL_STATUS)" >&2
      report_start_failure
      break
    fi
    if (( READY_ATTEMPT == 20 )); then
      echo "kernel process is running; waiting for its administrative socket" >&2
    fi
    sleep 0.1
  done
  if [[ "$READY" != true ]]; then
    exit 1
  fi
fi

if ! "$ADMIN" --root "$INSTANCE_ROOT" runtime status >/dev/null 2>&1; then
  echo "runtime subsystem is degraded; use 'runtime doctor' in the console for details" >&2
fi

set +e
"$ADMIN" --root "$INSTANCE_ROOT"
ADMIN_STATUS=$?
set -e

if [[ "$STARTED_KERNEL" == true ]]; then
  stop_started_kernel
fi
restore_terminal
trap - EXIT
exit "$ADMIN_STATUS"
