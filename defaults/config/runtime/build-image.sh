#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
IMAGE_DEFINITION=${2:-"$RUNTIME_SOURCE/image"}
IMAGE_ROOT=${3:-"$SOURCE_ROOT/node/kernel/runtime/images/full"}
MANIFEST=${4:-"$RUNTIME_SOURCE/versions.toml"}
RUNTIME_ROOT=${5:-"$SOURCE_ROOT/node/kernel/runtime"}
INSTANCE_UUID=${6:-}
if [[ -z "$SOURCE_ROOT" || ! -f "$MANIFEST" || ! -f "$IMAGE_DEFINITION/Containerfile" || ! -f "$IMAGE_DEFINITION/build.sh" || ! -f "$IMAGE_DEFINITION/deno.json" || ! -f "$IMAGE_DEFINITION/deno.lock" || -z "$INSTANCE_UUID" ]]; then
  echo "usage: defaults/config/runtime/build-image.sh <source-root> [image-definition] [image-root] [versions-file] [work-root] <instance-uuid>" >&2
  exit 2
fi
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "runtime image build/import requires root" >&2
  exit 1
fi

BUILDKIT_BIN="$RUNTIME_ROOT/buildkit/bin"
if [[ ! -x "$BUILDKIT_BIN/buildctl" || ! -x "$BUILDKIT_BIN/buildkitd" ]]; then
  BUILDKIT_BIN=$(dirname "$(command -v buildctl || true)")
fi
if [[ ! -x "$BUILDKIT_BIN/buildctl" || ! -x "$BUILDKIT_BIN/buildkitd" ]]; then
  echo "buildctl and buildkitd are required to build the full runtime image" >&2
  exit 1
fi
SMOKE_RECORD="$IMAGE_ROOT/smoke.json"
CONTEXT_ROOT="$RUNTIME_ROOT/service-context"
mkdir -p "$IMAGE_ROOT" "$RUNTIME_ROOT/buildkit/state" "$RUNTIME_ROOT/tmp"
CTR_BIN=$(command -v ctr || true)
if [[ -z "$CTR_BIN" ]]; then echo "ctr is required to build/import the runtime image" >&2; exit 1; fi

toml_value() {
  local section=$1 key=$2
  awk -v wanted_section="[$section]" -v wanted_key="$key" '
    /^\[/ { section=$0 }
    section == wanted_section && $1 == wanted_key && $2 == "=" {
      value=$0; sub(/^[^=]+=[[:space:]]*/, "", value); gsub(/^"|"$/, "", value); print value; exit
    }
  ' "$MANIFEST"
}

case $(uname -m) in
  x86_64|amd64) BASE_MANIFEST=$(toml_value deno base_manifest_digest_amd64) ;;
  aarch64|arm64) BASE_MANIFEST=$(toml_value deno base_manifest_digest_arm64) ;;
  *) echo "unsupported runtime architecture: $(uname -m)" >&2; exit 1 ;;
esac

IMAGE_NAME=$(toml_value runtime_image name)
IMAGE_RECORD="$IMAGE_ROOT/image.json"
IMAGE_SCHEMA=$(awk -F' = ' '$1 == "runtime_image_schema_version" { print $2; exit }' "$MANIFEST")
RUNTIME_PROTOCOL=$(awk -F' = ' '$1 == "runtime_protocol_version" { print $2; exit }' "$MANIFEST")
DENO_VERSION=$(toml_value deno version)
BASE_IMAGE=$(toml_value deno base_image)
BASE_DIGEST=$(toml_value deno base_image_digest)
NAMESPACE="the8020-$INSTANCE_UUID"

SOURCE_HASH=$(
  find "$RUNTIME_SOURCE/deno/supervisor" "$RUNTIME_SOURCE/deno/worker" "$RUNTIME_SOURCE/deno/kernel" "$RUNTIME_SOURCE/deno/context" "$RUNTIME_SOURCE/deno/http" -maxdepth 1 -type f \( -name '*.ts' -o -name '*.d.ts' \) ! -name '*_test.ts' -print0 | sort -z | xargs -0 sha256sum
  sha256sum "$RUNTIME_SOURCE/deno/deno.json" "$RUNTIME_SOURCE/deno/deno.lock" "$MANIFEST" "$IMAGE_DEFINITION/Containerfile" "$IMAGE_DEFINITION/build.sh" "$IMAGE_DEFINITION/deno.json" "$IMAGE_DEFINITION/deno.lock" "$RUNTIME_SOURCE/build-image.sh" "$RUNTIME_SOURCE/stage-service-runtime.sh" "$RUNTIME_SOURCE/bundle-runtime.sh" "$RUNTIME_SOURCE/protocol/generated.ts"
  printf '%s\n' "$BASE_MANIFEST"
)
SOURCE_HASH="sha256:$(printf '%s' "$SOURCE_HASH" | sha256sum | awk '{print $1}')"

if [[ -f "$IMAGE_RECORD" ]] && grep -Fq "\"source_hash\": \"$SOURCE_HASH\"" "$IMAGE_RECORD"; then
  RECORDED_DIGEST=$(awk -F'"' '/"digest":/ { print $4; exit }' "$IMAGE_RECORD")
  if "$CTR_BIN" --namespace "$NAMESPACE" images list | awk -v name="$IMAGE_NAME" '$1 == name { print $3 }' | grep -Fxq "$RECORDED_DIGEST" && \
    [[ -f "$SMOKE_RECORD" ]] && grep -Fq "\"image_digest\": \"$RECORDED_DIGEST\"" "$SMOKE_RECORD" && \
    grep -Fq '"runtime": "io.containerd.runsc.v1"' "$SMOKE_RECORD"; then
    echo "runtime image: unchanged input digest; reusing verified full image" >&2
    exit 0
  fi
fi

BUILDKIT_SOCKET="$RUNTIME_ROOT/buildkit/buildkitd.sock"
if ! "$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null 2>&1; then
  rm -f -- "$BUILDKIT_SOCKET"
  nohup "$BUILDKIT_BIN/buildkitd" --addr "unix://$BUILDKIT_SOCKET" --root "$RUNTIME_ROOT/buildkit/state" >"$RUNTIME_ROOT/buildkit/buildkitd.log" 2>&1 &
  BUILDKIT_PID=$!
  for _ in $(seq 1 200); do
    if "$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null 2>&1; then break; fi
    if ! kill -0 "$BUILDKIT_PID" 2>/dev/null; then wait "$BUILDKIT_PID" || true; echo "BuildKit failed to start" >&2; exit 1; fi
    sleep 0.05
  done
fi
"$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" debug workers >/dev/null

rm -rf -- "$CONTEXT_ROOT"
install -d -m 0700 "$CONTEXT_ROOT"
"$RUNTIME_SOURCE/stage-service-runtime.sh" "$SOURCE_ROOT" "$CONTEXT_ROOT"
install -m 0444 "$IMAGE_DEFINITION/deno.json" "$CONTEXT_ROOT/deno.json"
install -m 0444 "$IMAGE_DEFINITION/deno.lock" "$CONTEXT_ROOT/deno.lock"
install -m 0444 "$IMAGE_DEFINITION/build.sh" "$CONTEXT_ROOT/build.sh"
install -m 0555 "$RUNTIME_SOURCE/bundle-runtime.sh" "$CONTEXT_ROOT/bundle-runtime.sh"
install -m 0444 "$RUNTIME_SOURCE/protocol/generated.ts" "$CONTEXT_ROOT/protocol.ts"

OCI_ARCHIVE="$IMAGE_ROOT/deno-runtime.oci.tar"
DIGEST_FILE="$IMAGE_ROOT/deno-runtime.digest"
BUILD_HASH_FILE="$IMAGE_ROOT/deno-runtime.build-hash"
rm -f -- "$OCI_ARCHIVE"
echo "runtime image [full 1/3]: building generic image inside BuildKit" >&2
"$BUILDKIT_BIN/buildctl" --addr "unix://$BUILDKIT_SOCKET" build \
  --frontend dockerfile.v0 \
  --local context="$CONTEXT_ROOT" \
  --local dockerfile="$IMAGE_DEFINITION" \
  --opt filename=Containerfile \
  --opt "build-arg:DENO_BASE=$BASE_IMAGE@$BASE_MANIFEST" \
  --output "type=oci,name=$IMAGE_NAME,dest=$OCI_ARCHIVE"

"$CTR_BIN" --namespace "$NAMESPACE" images import --no-unpack --index-name "$IMAGE_NAME" "$OCI_ARCHIVE" >/dev/null
IMAGE_DIGEST=$("$CTR_BIN" --namespace "$NAMESPACE" images list | awk -v name="$IMAGE_NAME" '$1 == name { print $3; exit }')
if [[ ! "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]; then echo "containerd returned invalid runtime image digest: $IMAGE_DIGEST" >&2; exit 1; fi

SMOKE_ID="the8020-smoke-$$"
echo "runtime image [full 2/3]: smoke-testing imported gVisor image" >&2
SMOKE_NAMESPACE="the8020-smoke-$INSTANCE_UUID-$$-$(date +%s)"
SUPERVISOR_PORT=18000
while ss -H -ltn "sport = :$SUPERVISOR_PORT" | grep -q .; do SUPERVISOR_PORT=$((SUPERVISOR_PORT + 1)); done
INSPECTOR_PORT=$((SUPERVISOR_PORT + 1000))
while ss -H -ltn "sport = :$INSPECTOR_PORT" | grep -q .; do INSPECTOR_PORT=$((INSPECTOR_PORT + 1)); done
SMOKE_TOKEN=$(printf '%064d' 1)
cleanup_smoke() {
  "$CTR_BIN" --namespace "$SMOKE_NAMESPACE" tasks kill --signal SIGKILL --all "$SMOKE_ID" >/dev/null 2>&1 || true
  "$CTR_BIN" --namespace "$SMOKE_NAMESPACE" tasks delete --force "$SMOKE_ID" >/dev/null 2>&1 || true
  "$CTR_BIN" --namespace "$SMOKE_NAMESPACE" containers delete "$SMOKE_ID" >/dev/null 2>&1 || true
  "$CTR_BIN" --namespace "$SMOKE_NAMESPACE" snapshots remove "$SMOKE_ID" >/dev/null 2>&1 || true
  "$CTR_BIN" --namespace "$SMOKE_NAMESPACE" images remove "$IMAGE_NAME" >/dev/null 2>&1 || true
  "$CTR_BIN" namespaces remove "$SMOKE_NAMESPACE" >/dev/null 2>&1 || true
}
trap cleanup_smoke EXIT
"$CTR_BIN" namespaces create "$SMOKE_NAMESPACE"
"$CTR_BIN" --namespace "$SMOKE_NAMESPACE" images import --no-unpack --index-name "$IMAGE_NAME" "$OCI_ARCHIVE" >/dev/null
"$CTR_BIN" --namespace "$SMOKE_NAMESPACE" run --detach --runtime io.containerd.runsc.v1 \
  --runtime-config-path "$RUNTIME_SOURCE/runsc.toml" --net-host --read-only --null-io \
  --mount type=tmpfs,dst=/tmp,options=nosuid:nodev:noexec:mode=1777:size=67108864 \
  --mount type=tmpfs,dst=/runtime-cache,options=nosuid:nodev:noexec:mode=1777:size=67108864 \
  --env "SANDBOX_ID=$SMOKE_ID" --env "RUNTIME_GROUP_ID=$SMOKE_ID" --env WORKLOAD_TYPE=job \
  --env "IMAGE_DIGEST=$IMAGE_DIGEST" --env "INTERNAL_API_TOKEN=$SMOKE_TOKEN" \
  --env "SUPERVISOR_PORT=$SUPERVISOR_PORT" --env "INSPECTOR_PORT=$INSPECTOR_PORT" --env RUNTIME_PROFILE_HASH=smoke \
  "$IMAGE_NAME" "$SMOKE_ID" deno run --unstable-worker-options --config=/opt/runtime/deno.json --cached-only --no-prompt \
  "--inspect=127.0.0.1:$INSPECTOR_PORT" --allow-read=/opt/runtime --allow-write=/tmp,/runtime-cache \
  "--allow-net=0.0.0.0:$SUPERVISOR_PORT" \
  --allow-env=SANDBOX_ID,RUNTIME_GROUP_ID,WORKLOAD_TYPE,IMAGE_DIGEST,INTERNAL_API_TOKEN,SUPERVISOR_PORT,INSPECTOR_PORT,RUNTIME_PROFILE_HASH \
  /opt/runtime/supervisor/main.ts
"$CTR_BIN" --namespace "$SMOKE_NAMESPACE" containers info "$SMOKE_ID" | grep -Fq io.containerd.runsc.v1
for _ in $(seq 1 200); do
  if curl --fail --silent --header "Authorization: Bearer $SMOKE_TOKEN" "http://127.0.0.1:$SUPERVISOR_PORT/v1/status" > "$RUNTIME_ROOT/tmp/smoke-status.json"; then break; fi
  sleep 0.05
done
grep -Fq "\"deno_version\":\"$DENO_VERSION\"" "$RUNTIME_ROOT/tmp/smoke-status.json"
curl --fail --silent --header "Authorization: Bearer $SMOKE_TOKEN" --header 'Content-Type: application/json' \
  --data "{\"protocol_version\":$RUNTIME_PROTOCOL,\"message_type\":\"start_worker\",\"runtime_group_id\":\"$SMOKE_ID\",\"correlation_id\":\"smoke-start\",\"payload\":{\"metadata\":{\"workerId\":\"worker-smoke\",\"executionId\":\"execution-smoke\",\"workloadType\":\"job\",\"ownerId\":\"installer\",\"workloadId\":\"smoke\",\"releaseId\":\"$IMAGE_DIGEST\",\"entrypoint\":\"file:///opt/runtime/worker/smoke.ts\",\"debuggerName\":\"job:installer:execution-smoke:worker-smoke\"},\"permissions\":{\"read\":[\"/opt/runtime\"]}}}" \
  "http://127.0.0.1:$SUPERVISOR_PORT/v1/workers/start" > "$RUNTIME_ROOT/tmp/smoke-worker.json"
grep -Fq '"worker_id":"worker-smoke"' "$RUNTIME_ROOT/tmp/smoke-worker.json"
grep -Fq '"message_type":"worker_state_change"' "$RUNTIME_ROOT/tmp/smoke-worker.json"
curl --fail --silent "http://127.0.0.1:$INSPECTOR_PORT/json/list" | grep -Fq 'job:installer:execution-smoke:worker-smoke'
curl --fail --silent --header "Authorization: Bearer $SMOKE_TOKEN" --header 'Content-Type: application/json' \
  --data "{\"protocol_version\":$RUNTIME_PROTOCOL,\"message_type\":\"job_start\",\"runtime_group_id\":\"$SMOKE_ID\",\"correlation_id\":\"smoke-job\",\"payload\":{\"arguments\":[{\"value\":1}],\"secrets\":{}}}" \
  "http://127.0.0.1:$SUPERVISOR_PORT/v1/jobs/worker-smoke/run" > "$RUNTIME_ROOT/tmp/smoke-result.json"
grep -Fq '"smoke":true' "$RUNTIME_ROOT/tmp/smoke-result.json"
grep -Fq '"message_type":"job_result"' "$RUNTIME_ROOT/tmp/smoke-result.json"
curl --fail --silent --request POST --header "Authorization: Bearer $SMOKE_TOKEN" --header 'Content-Type: application/json' \
  --data "{\"protocol_version\":$RUNTIME_PROTOCOL,\"message_type\":\"stop_worker\",\"runtime_group_id\":\"$SMOKE_ID\",\"correlation_id\":\"smoke-stop\",\"payload\":{\"immediate\":false}}" \
  "http://127.0.0.1:$SUPERVISOR_PORT/v1/workers/worker-smoke/stop" > "$RUNTIME_ROOT/tmp/smoke-stop.json"
grep -Fq '"message_type":"worker_state_change"' "$RUNTIME_ROOT/tmp/smoke-stop.json"
cleanup_smoke
if "$CTR_BIN" namespaces list -q | grep -Fxq "$SMOKE_NAMESPACE"; then
  echo "runtime smoke namespace cleanup failed: $SMOKE_NAMESPACE" >&2
  exit 1
fi
trap - EXIT

BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "runtime image [full 3/3]: publishing verified image records" >&2
TEMP_RECORD="$IMAGE_RECORD.tmp"
mkdir -p "$(dirname "$IMAGE_RECORD")"
printf '{\n  "schema_version": %s,\n  "name": "%s",\n  "digest": "%s",\n  "base_digest": "%s",\n  "deno_version": "%s",\n  "source_hash": "%s",\n  "built_at": "%s"\n}\n' \
  "$IMAGE_SCHEMA" "$IMAGE_NAME" "$IMAGE_DIGEST" "$BASE_DIGEST" "$DENO_VERSION" "$SOURCE_HASH" "$BUILT_AT" > "$TEMP_RECORD"
chmod 0600 "$TEMP_RECORD"
mv -f -- "$TEMP_RECORD" "$IMAGE_RECORD"
printf '%s\n' "$IMAGE_DIGEST" > "$DIGEST_FILE"
printf '%s\n' "$SOURCE_HASH" > "$BUILD_HASH_FILE"
chmod 0600 "$DIGEST_FILE" "$BUILD_HASH_FILE"
printf '{\n  "passed_at": "%s",\n  "image_digest": "%s",\n  "runtime": "io.containerd.runsc.v1"\n}\n' "$BUILT_AT" "$IMAGE_DIGEST" > "$SMOKE_RECORD"
chmod 0600 "$SMOKE_RECORD"
