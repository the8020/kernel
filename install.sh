#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=$(pwd -P)
SETUP_RUNTIME_HOST=true
RUN_VERIFICATION=true

for argument in "$@"; do
  case "$argument" in
    --instance-root=*) INSTANCE_ROOT=${argument#--instance-root=} ;;
    --skip-runtime-host) SETUP_RUNTIME_HOST=false ;;
    --skip-verification) RUN_VERIFICATION=false ;;
    --help)
      echo "usage: ./install.sh [--instance-root=<path>] [--skip-runtime-host] [--skip-verification]"
      exit 0
      ;;
    *) echo "unknown install option: $argument" >&2; exit 2 ;;
  esac
done

if [[ ! -f "$SOURCE_ROOT/AGENTS.md" || ! -f "$SOURCE_ROOT/go.mod" || ! -d "$SOURCE_ROOT/kernel/cbus/gen" ]]; then
  echo "80|20 installation source is incomplete: $SOURCE_ROOT" >&2
  exit 1
fi
if ! command -v git >/dev/null 2>&1; then
  echo "git is required to inspect and synchronize 80|20 packages" >&2
  exit 1
fi
if ! git --version >/dev/null 2>&1; then
  echo "git is installed but cannot be executed" >&2
  exit 1
fi
if [[ ! -d "$INSTANCE_ROOT" ]]; then
  echo "instance root does not exist: $INSTANCE_ROOT" >&2
  exit 1
fi
INSTANCE_ROOT=$(cd -- "$INSTANCE_ROOT" && pwd -P)
cd "$SOURCE_ROOT"

DEVELOPMENT_DIR="$SOURCE_ROOT/.development"
TOOLCHAIN_DIR="$DEVELOPMENT_DIR/toolchains/go"
TEMP_DIR="$DEVELOPMENT_DIR/tmp"
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"

mkdir -p \
  "$DEVELOPMENT_DIR/cache/go-build" \
  "$DEVELOPMENT_DIR/cache/go-mod" \
  "$DEVELOPMENT_DIR/bin" \
  "$DEVELOPMENT_DIR/generated" \
  "$TEMP_DIR"

GO_VERSION=$(tr -d '[:space:]' < "$SOURCE_ROOT/.go-version")
if [[ ! "$GO_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo ".go-version must contain a full Go version" >&2
  exit 1
fi

go_is_compatible() {
  local candidate=${1#go}
  local required=$2
  local candidate_major candidate_minor candidate_patch
  local required_major required_minor required_patch
  IFS=. read -r candidate_major candidate_minor candidate_patch <<< "$candidate"
  IFS=. read -r required_major required_minor required_patch <<< "$required"
  [[ "$candidate_major" =~ ^[0-9]+$ && "$candidate_minor" =~ ^[0-9]+$ && "$candidate_patch" =~ ^[0-9]+$ ]] || return 1
  (( candidate_major > required_major )) ||
    (( candidate_major == required_major && candidate_minor > required_minor )) ||
    (( candidate_major == required_major && candidate_minor == required_minor && candidate_patch >= required_patch ))
}

GO_CMD=""
if command -v go >/dev/null 2>&1 && go_is_compatible "$(go env GOVERSION 2>/dev/null || true)" "$GO_VERSION"; then
  GO_CMD=$(command -v go)
elif [[ -x "$TOOLCHAIN_DIR/bin/go" && "$($TOOLCHAIN_DIR/bin/go env GOVERSION 2>/dev/null || true)" == "go$GO_VERSION" ]]; then
  GO_CMD="$TOOLCHAIN_DIR/bin/go"
else
  case "$(uname -s)-$(uname -m)" in
    Linux-x86_64)
      PLATFORM="linux-amd64"
      CHECKSUM="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
      ;;
    Linux-aarch64|Linux-arm64)
      PLATFORM="linux-arm64"
      CHECKSUM="fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49"
      ;;
    Darwin-x86_64)
      PLATFORM="darwin-amd64"
      CHECKSUM="6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf725"
      ;;
    Darwin-arm64)
      PLATFORM="darwin-arm64"
      CHECKSUM="efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a"
      ;;
    *)
      echo "no local Go toolchain download is defined for $(uname -s)-$(uname -m)" >&2
      exit 1
      ;;
  esac

  ARCHIVE="$TEMP_DIR/go$GO_VERSION.$PLATFORM.tar.gz"
  URL="https://go.dev/dl/go$GO_VERSION.$PLATFORM.tar.gz"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --retry 3 --output "$ARCHIVE" "$URL"
  elif command -v wget >/dev/null 2>&1; then
    wget --output-document="$ARCHIVE" "$URL"
  else
    echo "curl or wget is required to download the local Go toolchain" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$CHECKSUM" "$ARCHIVE" | sha256sum --check --status
  else
    [[ "$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')" == "$CHECKSUM" ]]
  fi
  if [[ -e "$TOOLCHAIN_DIR" ]]; then
    rm -rf -- "$TOOLCHAIN_DIR"
  fi
  mkdir -p "$(dirname "$TOOLCHAIN_DIR")"
  tar -xzf "$ARCHIVE" -C "$(dirname "$TOOLCHAIN_DIR")"
  rm -f -- "$ARCHIVE"
  GO_CMD="$TOOLCHAIN_DIR/bin/go"
fi

export GOCACHE="$DEVELOPMENT_DIR/cache/go-build"
export GOMODCACHE="$DEVELOPMENT_DIR/cache/go-mod"
export GOWORK=off

echo "platform build [1/5]: generating protocols and building Go binaries" >&2
"$GO_CMD" mod download
"$GO_CMD" mod verify
"$GO_CMD" run ./kernel/cbus/gen
GOFMT_CMD="$($GO_CMD env GOROOT)/bin/gofmt"
find "$DEVELOPMENT_DIR/generated" -name '*.go' -type f -print0 | xargs -0 "$GOFMT_CMD" -w
"$GO_CMD" -C "$DEVELOPMENT_DIR/generated" list -mod=mod ./... >/dev/null
"$GO_CMD" -C "$DEVELOPMENT_DIR/generated" build -mod=readonly -trimpath -o "$DEVELOPMENT_DIR/bin/kernel" ./cmd/kernel
"$GO_CMD" -C "$DEVELOPMENT_DIR/generated" build -mod=readonly -trimpath -o "$DEVELOPMENT_DIR/bin/admin" ./cmd/admin

KERNEL="$DEVELOPMENT_DIR/bin/kernel"
LAYOUT_FILE="$INSTANCE_ROOT/node/kernel/paths.toml"
NEW_LAYOUT=false
if [[ ! -f "$LAYOUT_FILE" ]]; then
  NEW_LAYOUT=true
  echo "platform build [2/5]: initializing instance layout" >&2
  "$KERNEL" --root "$INSTANCE_ROOT" --init-defaults --init-only
else
  echo "platform build [2/5]: using initialized instance layout" >&2
fi
if [[ ! -f "$LAYOUT_FILE" ]]; then
  echo "kernel initialization did not create $LAYOUT_FILE" >&2
  exit 1
fi

layout_path() {
  local key=$1
  sed -nE "s/^${key}[[:space:]]*=[[:space:]]*['\"]([^'\"]+)['\"]$/\\1/p" "$LAYOUT_FILE" | head -n 1
}

CONFIG_ROOT=$(layout_path config)
STATE_ROOT=$(layout_path state)
PACKAGES_ROOT=$(layout_path packages)
if [[ -z "$CONFIG_ROOT" || ! -d "$CONFIG_ROOT" || -z "$STATE_ROOT" || ! -d "$STATE_ROOT" || -z "$PACKAGES_ROOT" || ! -d "$PACKAGES_ROOT" ]]; then
  echo "initialized layout has invalid configuration, state, or package roots" >&2
  exit 1
fi
CONFIG_RUNTIME="$CONFIG_ROOT/runtime"
NODE_KERNEL="$INSTANCE_ROOT/node/kernel"
NODE_RUNTIME="$NODE_KERNEL/runtime"
mkdir -p "$CONFIG_RUNTIME" "$NODE_RUNTIME/images/rootless" "$NODE_RUNTIME/images/full" "$NODE_RUNTIME/images/development" "$NODE_KERNEL/bin"

# Installation, not the kernel, owns repository defaults. Shared and node-local
# operator files are created only when absent; the two empty files created by a
# brand-new layout receive their initial schema templates here.
if [[ "$NEW_LAYOUT" == true ]]; then
  install -m 0600 "$SOURCE_ROOT/defaults/config/settings.toml" "$CONFIG_ROOT/settings.toml"
  install -m 0600 "$SOURCE_ROOT/defaults/node/kernel/settings.toml" "$NODE_KERNEL/settings.toml"
fi
while IFS= read -r -d '' directory; do
  relative=${directory#"$SOURCE_ROOT/defaults/config"/}
  [[ "$directory" == "$SOURCE_ROOT/defaults/config" ]] && relative=""
  [[ "$relative" == runtime || "$relative" == runtime/* ]] && continue
  mkdir -p "$CONFIG_ROOT/$relative"
done < <(find "$SOURCE_ROOT/defaults/config" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$SOURCE_ROOT/defaults/config"/}
  [[ "$relative" == runtime/* ]] && continue
  destination="$CONFIG_ROOT/$relative"
  if [[ ! -e "$destination" ]]; then
    install -m 0600 "$source" "$destination"
  fi
done < <(find "$SOURCE_ROOT/defaults/config" -type f -print0)
while IFS= read -r -d '' directory; do
  relative=${directory#"$SOURCE_ROOT/defaults/node"/}
  [[ "$directory" == "$SOURCE_ROOT/defaults/node" ]] && relative=""
  mkdir -p "$INSTANCE_ROOT/node/$relative"
done < <(find "$SOURCE_ROOT/defaults/node" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$SOURCE_ROOT/defaults/node"/}
  destination="$INSTANCE_ROOT/node/$relative"
  if [[ ! -e "$destination" ]]; then
    install -m 0600 "$source" "$destination"
  fi
done < <(find "$SOURCE_ROOT/defaults/node" -type f -print0)

# Package defaults establish desired state only for an empty index. Once any
# package has been declared, the shared index is entirely operator-owned.
PACKAGE_INDEX_ROOT="$STATE_ROOT/package-index"
mkdir -p "$PACKAGE_INDEX_ROOT"
if ! find "$PACKAGE_INDEX_ROOT" -mindepth 2 -maxdepth 2 -type f -name '*.toml' -print -quit | grep -q .; then
  while IFS= read -r -d '' directory; do
    relative=${directory#"$SOURCE_ROOT/defaults/state/package-index"/}
    [[ "$directory" == "$SOURCE_ROOT/defaults/state/package-index" ]] && relative=""
    install -d -m 0755 "$PACKAGE_INDEX_ROOT/$relative"
  done < <(find "$SOURCE_ROOT/defaults/state/package-index" -type d -print0)
  while IFS= read -r -d '' source; do
    relative=${source#"$SOURCE_ROOT/defaults/state/package-index"/}
    install -m 0600 "$source" "$PACKAGE_INDEX_ROOT/$relative"
  done < <(find "$SOURCE_ROOT/defaults/state/package-index" -type f -name '*.toml' -print0)
fi

# Seed package content exactly once as part of a fresh instance installation.
# Existing instances update packages only through an explicit synchronization.
if [[ "$NEW_LAYOUT" == true ]]; then
  echo "synchronizing initial indexed packages" >&2
  "$KERNEL" --root "$INSTANCE_ROOT" --init-only --synchronize-packages
fi

# Runtime definitions and source are one platform-owned build-input tree.
# Replace it atomically so removed tracked files cannot survive indefinitely in
# an existing instance; other shared configuration remains untouched.
CONFIG_RUNTIME_STAGE=$(mktemp -d "$CONFIG_ROOT/.runtime-install.XXXXXX")
CONFIG_RUNTIME_PREVIOUS=""
cleanup_runtime_refresh() {
  if [[ -n "$CONFIG_RUNTIME_STAGE" && -e "$CONFIG_RUNTIME_STAGE" ]]; then
    rm -rf -- "$CONFIG_RUNTIME_STAGE"
  fi
  if [[ -n "$CONFIG_RUNTIME_PREVIOUS" && -e "$CONFIG_RUNTIME_PREVIOUS" && ! -e "$CONFIG_RUNTIME" ]]; then
    mv -- "$CONFIG_RUNTIME_PREVIOUS" "$CONFIG_RUNTIME"
  fi
}
trap cleanup_runtime_refresh EXIT
chmod 0755 "$CONFIG_RUNTIME_STAGE"
while IFS= read -r -d '' directory; do
  relative=${directory#"$RUNTIME_SOURCE"/}
  [[ "$directory" == "$RUNTIME_SOURCE" ]] && relative=""
  install -d -m 0755 "$CONFIG_RUNTIME_STAGE/$relative"
done < <(find "$RUNTIME_SOURCE" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$RUNTIME_SOURCE"/}
  install -m 0644 "$source" "$CONFIG_RUNTIME_STAGE/$relative"
done < <(find "$RUNTIME_SOURCE" -type f -print0)
if [[ -e "$CONFIG_RUNTIME" ]]; then
  CONFIG_RUNTIME_PREVIOUS="$CONFIG_ROOT/.runtime-previous.$$"
  if [[ -e "$CONFIG_RUNTIME_PREVIOUS" ]]; then
    echo "runtime configuration backup path already exists: $CONFIG_RUNTIME_PREVIOUS" >&2
    exit 1
  fi
  mv -- "$CONFIG_RUNTIME" "$CONFIG_RUNTIME_PREVIOUS"
fi
if ! mv -- "$CONFIG_RUNTIME_STAGE" "$CONFIG_RUNTIME"; then
  exit 1
fi
CONFIG_RUNTIME_STAGE=""
if [[ -n "$CONFIG_RUNTIME_PREVIOUS" ]]; then
  rm -rf -- "$CONFIG_RUNTIME_PREVIOUS"
  CONFIG_RUNTIME_PREVIOUS=""
fi
trap - EXIT

if [[ ! -f "$CONFIG_RUNTIME/versions.toml" || ! -f "$CONFIG_RUNTIME/image/Containerfile" || ! -f "$CONFIG_RUNTIME/image/deno.json" ]]; then
  echo "instance runtime configuration is incomplete: $CONFIG_RUNTIME" >&2
  exit 1
fi

echo "platform build [3/5]: refreshing portable generic runtime image" >&2
bash "$RUNTIME_SOURCE/install-portable.sh" \
  "$SOURCE_ROOT" \
  "$NODE_RUNTIME/images/rootless" \
  "$CONFIG_RUNTIME/versions.toml" \
  "$CONFIG_RUNTIME/image/Containerfile" \
  "$CONFIG_RUNTIME/image/deno.json" \
  "$NODE_RUNTIME" \
  "$NODE_KERNEL/bin/runsc"

echo "platform build [4/5]: refreshing portable development image" >&2
bash "$RUNTIME_SOURCE/development/install-portable.sh" \
  "$SOURCE_ROOT" \
  "$CONFIG_RUNTIME/development" \
  "$NODE_RUNTIME/images/development" \
  "$CONFIG_RUNTIME/versions.toml" \
  "$NODE_RUNTIME"

has_effective_capability() {
  local bit=$1 hex nibble_index nibble mask
  hex=$(awk '$1 == "CapEff:" { print $2; exit }' /proc/self/status 2>/dev/null || true)
  [[ "$hex" =~ ^[0-9a-fA-F]+$ ]] || return 1
  nibble_index=$(( ${#hex} - 1 - bit / 4 ))
  (( nibble_index >= 0 )) || return 1
  nibble=$((16#${hex:nibble_index:1}))
  mask=$((1 << (bit % 4)))
  (( (nibble & mask) != 0 ))
}

full_runtime_host_available() {
  [[ $(uname -s) == Linux && ${EUID:-$(id -u)} -eq 0 ]] || return 1
  [[ -f /sys/fs/cgroup/cgroup.controllers ]] || return 1
  awk '$2 == "/sys/fs/cgroup" && $4 ~ /(^|,)rw(,|$)/ { found=1 } END { exit !found }' /proc/mounts || return 1
  has_effective_capability 21 || return 1
  has_effective_capability 12 || return 1
}

FULL_RUNTIME_HOST=false
REQUESTED_RUNTIME_MODE=${KERNEL_SANDBOX_RUNTIME_MODE:-auto}
if [[ "$SETUP_RUNTIME_HOST" == true && "$REQUESTED_RUNTIME_MODE" != rootless ]] && full_runtime_host_available; then
  bash "$RUNTIME_SOURCE/install-host.sh" "$SOURCE_ROOT" "$CONFIG_RUNTIME/versions.toml" "$NODE_RUNTIME"
  FULL_RUNTIME_HOST=true
elif [[ "$REQUESTED_RUNTIME_MODE" == full ]]; then
  echo "full sandbox mode was requested but SYS_ADMIN, NET_ADMIN, and writable cgroup v2 authority are unavailable" >&2
  exit 1
else
  echo "sandbox install mode: rootless gVisor (full host authority unavailable or skipped)" >&2
fi

if [[ "$FULL_RUNTIME_HOST" == true ]]; then
  INSTANCE_UUID=$(sed -nE 's/^uuid[[:space:]]*=[[:space:]]*"([^"]+)"$/\1/p' "$NODE_KERNEL/instance.toml" | head -n 1)
  if [[ -z "$INSTANCE_UUID" ]]; then
    echo "node identity is missing" >&2
    exit 1
  fi
  bash "$RUNTIME_SOURCE/build-image.sh" \
    "$SOURCE_ROOT" "$CONFIG_RUNTIME/image" "$NODE_RUNTIME/images/full" \
    "$CONFIG_RUNTIME/versions.toml" "$NODE_RUNTIME" "$INSTANCE_UUID"
  bash "$RUNTIME_SOURCE/development/build-image.sh" \
    "$SOURCE_ROOT" "$CONFIG_RUNTIME/development" "$NODE_RUNTIME/images/development" \
    "$CONFIG_RUNTIME/versions.toml" "$NODE_RUNTIME"
fi

echo "platform build [5/5]: verifying platform artifacts" >&2
if [[ "$RUN_VERIFICATION" == true ]]; then
  "$GO_CMD" test ./kernel/...
  "$GO_CMD" -C "$DEVELOPMENT_DIR/generated" test -mod=readonly ./...

  DENO_CMD="$NODE_RUNTIME/images/rootless/rootfs/usr/bin/deno"
  if [[ ! -x "$DENO_CMD" ]]; then
    echo "verified portable runtime does not contain Deno" >&2
    exit 1
  fi
  export DENO_DIR="$NODE_RUNTIME/verification-deno-cache"
  (cd "$RUNTIME_SOURCE/deno" && "$DENO_CMD" task check && "$DENO_CMD" task test)
  "$DENO_CMD" fmt --check "$RUNTIME_SOURCE/development/image/files"
  "$DENO_CMD" lint "$RUNTIME_SOURCE/development/image/files"
  "$DENO_CMD" check "$RUNTIME_SOURCE/development/image/files/activate.ts"
else
  echo "platform verification gates skipped; required binaries and images are current" >&2
fi
