#!/usr/bin/env bash
set -euo pipefail

RUNSC=${1:-}
ROOTFS=${2:-}
IMAGE_RECORD=${3:-}
SMOKE_RECORD=${4:-}
WORK_ROOT=${5:-}
if [[ ! -x "$RUNSC" || ! -x "$ROOTFS/usr/bin/deno" || ! -f "$IMAGE_RECORD" || -z "$SMOKE_RECORD" || -z "$WORK_ROOT" ]]; then
  echo "usage: smoke-portable.sh <runsc> <rootfs> <image-record> <smoke-record> <work-root>" >&2
  exit 2
fi
for path in "$RUNSC" "$ROOTFS" "$IMAGE_RECORD" "$SMOKE_RECORD" "$WORK_ROOT"; do
  [[ "$path" == /* ]] || { echo "portable smoke paths must be absolute" >&2; exit 2; }
done

IMAGE_DIGEST=$(sed -nE 's/^[[:space:]]*"image_digest":[[:space:]]*"(sha256:[0-9a-f]{64})",?$/\1/p' "$IMAGE_RECORD" | head -n 1)
if [[ -z "$IMAGE_DIGEST" ]]; then
  echo "portable runtime image record has no valid image digest" >&2
  exit 1
fi
if [[ -f "$SMOKE_RECORD" ]] &&
   grep -Fq "\"image_digest\": \"$IMAGE_DIGEST\"" "$SMOKE_RECORD" &&
   grep -Fq '"runtime": "runsc-rootless-systrap"' "$SMOKE_RECORD"; then
  exit 0
fi

mkdir -p "$WORK_ROOT" "$(dirname "$SMOKE_RECORD")"
SMOKE_STAGE=$(mktemp -d "$WORK_ROOT/rootless-smoke.XXXXXX")
SMOKE_ID="the8020-rootless-smoke-$$"
SMOKE_RUNSC_ROOT="$SMOKE_STAGE/runsc"
SMOKE_OVERLAY="$SMOKE_STAGE/overlay"
SMOKE_BUNDLE="$SMOKE_STAGE/bundle"
SMOKE_OUTPUT="$SMOKE_STAGE/output.log"
mkdir -p "$SMOKE_RUNSC_ROOT" "$SMOKE_OVERLAY" "$SMOKE_BUNDLE"

cleanup() {
  "$RUNSC" --root="$SMOKE_RUNSC_ROOT" --rootless=true --platform=systrap --network=none --overlay2="root:dir=$SMOKE_OVERLAY" delete --force "$SMOKE_ID" >/dev/null 2>&1 || true
  rm -rf -- "$SMOKE_STAGE"
}
trap cleanup EXIT

cat > "$SMOKE_BUNDLE/config.json" <<EOF
{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 1993, "gid": 1993},
    "args": ["/usr/bin/deno", "eval", "--config=/opt/runtime/deno.json", "--cached-only", "await import(\"@the8020/http\"); await import(\"@the8020/kernel\"); console.log(\"the8020-rootless-smoke\")"],
    "env": ["PATH=/usr/bin", "HOME=/tmp", "DENO_DIR=/tmp/deno-cache", "DENO_NO_UPDATE_CHECK=1", "DENO_NO_PROMPT=1"],
    "cwd": "/tmp",
    "capabilities": {"bounding": [], "effective": [], "inheritable": [], "permitted": [], "ambient": []},
    "noNewPrivileges": true
  },
  "root": {"path": "$ROOTFS", "readonly": false},
  "hostname": "rootless-smoke",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "mode=755"]},
    {"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": ["nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"]},
    {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "nodev", "mode=1777"]}
  ],
  "linux": {"namespaces": [{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}, {"type": "network"}]}
}
EOF

echo "first boot: smoke-testing the bundled rootless gVisor runtime" >&2
if ! "$RUNSC" \
  --allow-rootfs-tar-annotation --root="$SMOKE_RUNSC_ROOT" --rootless=true --platform=systrap --directfs=false \
  --file-access=exclusive --file-access-mounts=shared --network=none --overlay2="root:dir=$SMOKE_OVERLAY" \
  --log="$SMOKE_STAGE/runsc.log" run --bundle="$SMOKE_BUNDLE" "$SMOKE_ID" >"$SMOKE_OUTPUT" 2>&1; then
  echo "bundled rootless gVisor smoke test failed; run the container with --security-opt seccomp=unconfined" >&2
  tail -80 "$SMOKE_OUTPUT" >&2 || true
  tail -80 "$SMOKE_STAGE/runsc.log" >&2 || true
  exit 1
fi
grep -Fq the8020-rootless-smoke "$SMOKE_OUTPUT"

PASSED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TEMP_SMOKE="$SMOKE_RECORD.tmp.$$"
printf '{\n  "passed_at": "%s",\n  "image_digest": "%s",\n  "runtime": "runsc-rootless-systrap"\n}\n' \
  "$PASSED_AT" "$IMAGE_DIGEST" > "$TEMP_SMOKE"
chmod 0600 "$TEMP_SMOKE"
mv -f -- "$TEMP_SMOKE" "$SMOKE_RECORD"
echo "first boot: bundled rootless gVisor runtime is ready" >&2
