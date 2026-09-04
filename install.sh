#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
INSTANCE_ROOT=$(pwd -P)
SETUP_RUNTIME_HOST=true
RUN_VERIFICATION=true
RELEASE_VERSION=${THE8020_RELEASE_VERSION:-}

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
if [[ -n "$RELEASE_VERSION" && ! "$RELEASE_VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "THE8020_RELEASE_VERSION must contain exactly major.minor without leading zeroes" >&2
  exit 1
fi
if [[ -n "$RELEASE_VERSION" && -n "${THE8020_BOOTSTRAP_SOURCE_ROOT:-}" ]]; then
  echo "THE8020_RELEASE_VERSION cannot be combined with THE8020_BOOTSTRAP_SOURCE_ROOT" >&2
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
KERNEL_CONFIG="$INSTANCE_ROOT/kernel.toml"
NEW_LAYOUT=false
if [[ ! -f "$KERNEL_CONFIG" ]]; then
  NEW_LAYOUT=true
  echo "platform build [2/5]: initializing instance layout" >&2
  "$KERNEL" --root "$INSTANCE_ROOT" --init-defaults --init-only
else
  echo "platform build [2/5]: using initialized instance layout" >&2
fi
if [[ ! -f "$KERNEL_CONFIG" ]]; then
  echo "kernel initialization did not create $KERNEL_CONFIG" >&2
  exit 1
fi
PACKAGES_ROOT="$INSTANCE_ROOT/packages"
if [[ ! -d "$PACKAGES_ROOT" || ! -d "$INSTANCE_ROOT/users" ]]; then
  echo "initialized layout is missing its package or user root" >&2
  exit 1
fi
NODE_KERNEL="$INSTANCE_ROOT/node/kernel"
NODE_RUNTIME="$NODE_KERNEL/runtime"
RUNTIME_DEFINITIONS="$NODE_RUNTIME/definitions"
mkdir -p "$NODE_RUNTIME/images/rootless" "$NODE_RUNTIME/images/full" "$NODE_RUNTIME/images/development" "$NODE_KERNEL/bin"

# Development helper scripts are platform-owned and mounted read-only into
# every development sandbox. Refresh the complete tree so removed helpers do
# not survive an upgrade and retain only the executable bit declared by source.
SCRIPTS_SOURCE="$SOURCE_ROOT/defaults/scripts"
SCRIPTS_ROOT="$INSTANCE_ROOT/scripts"
SCRIPTS_STAGE=$(mktemp -d "$INSTANCE_ROOT/.scripts-install.XXXXXX")
SCRIPTS_PREVIOUS=""
cleanup_scripts_refresh() {
  if [[ -n "$SCRIPTS_STAGE" && -e "$SCRIPTS_STAGE" ]]; then
    rm -rf -- "$SCRIPTS_STAGE"
  fi
  if [[ -n "$SCRIPTS_PREVIOUS" && -e "$SCRIPTS_PREVIOUS" && ! -e "$SCRIPTS_ROOT" ]]; then
    mv -- "$SCRIPTS_PREVIOUS" "$SCRIPTS_ROOT"
  fi
}
trap cleanup_scripts_refresh EXIT
chmod 0755 "$SCRIPTS_STAGE"
while IFS= read -r -d '' directory; do
  relative=${directory#"$SCRIPTS_SOURCE"/}
  [[ "$directory" == "$SCRIPTS_SOURCE" ]] && relative=""
  install -d -m 0755 "$SCRIPTS_STAGE/$relative"
done < <(find "$SCRIPTS_SOURCE" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$SCRIPTS_SOURCE"/}
  if source_mode=$(stat -c '%a' -- "$source" 2>/dev/null); then
    :
  elif source_mode=$(stat -f '%Lp' "$source" 2>/dev/null); then
    :
  else
    echo "cannot inspect helper-script mode: $source" >&2
    exit 1
  fi
  if [[ ! "$source_mode" =~ ^[0-7]{3,4}$ ]]; then
    echo "invalid helper-script mode $source_mode: $source" >&2
    exit 1
  fi
  mode=0444
  (( (8#$source_mode & 8#111) != 0 )) && mode=0555
  install -m "$mode" "$source" "$SCRIPTS_STAGE/$relative"
done < <(find "$SCRIPTS_SOURCE" -type f -print0)
if [[ -e "$SCRIPTS_ROOT" ]]; then
  SCRIPTS_PREVIOUS="$INSTANCE_ROOT/.scripts-previous.$$"
  if [[ -e "$SCRIPTS_PREVIOUS" ]]; then
    echo "script backup path already exists: $SCRIPTS_PREVIOUS" >&2
    exit 1
  fi
  mv -- "$SCRIPTS_ROOT" "$SCRIPTS_PREVIOUS"
fi
mv -- "$SCRIPTS_STAGE" "$SCRIPTS_ROOT"
SCRIPTS_STAGE=""
if [[ -n "$SCRIPTS_PREVIOUS" ]]; then
  rm -rf -- "$SCRIPTS_PREVIOUS"
  SCRIPTS_PREVIOUS=""
fi
trap - EXIT

# A fresh node stages the immutable bootstrap package set once. The database
# records desired and active packages during first boot and is authoritative
# thereafter. Local source roots are a development/test input only.
if [[ "$NEW_LAYOUT" == true ]]; then
  BOOTSTRAP_SOURCE_ROOT=${THE8020_BOOTSTRAP_SOURCE_ROOT:-"$(dirname "$SOURCE_ROOT")"}
  while IFS=$'\t' read -r package_id package_source; do
    [[ -n "$package_id" && -n "$package_source" ]] || continue
    if [[ ! "$package_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
      echo "invalid bootstrap package ID: $package_id" >&2
      exit 1
    fi
    namespace=${package_id%%/*}
    repository=${package_id#*/}
    namespace_root="$PACKAGES_ROOT/$namespace"
    destination="$namespace_root/$repository"
    [[ ! -e "$destination" ]] || continue
    install -d -m 0755 "$namespace_root"
    package_stage=$(mktemp -d "$namespace_root/.${repository}.install.XXXXXX")
    local_source=""
    if [[ -z "$RELEASE_VERSION" ]]; then
      local_source="$BOOTSTRAP_SOURCE_ROOT/$repository"
      if [[ ! -f "$local_source/package.toml" ]]; then
        local_source="$BOOTSTRAP_SOURCE_ROOT/$namespace/$repository"
      fi
    fi
    if [[ -n "$local_source" && -f "$local_source/package.toml" ]]; then
      cp -a "$local_source/." "$package_stage/"
      rm -rf -- "$package_stage/.git" "$package_stage/.development" "$package_stage/node_modules"
    elif [[ -n "$RELEASE_VERSION" ]]; then
      resolution=$("$SOURCE_ROOT/release-tag.sh" package "$RELEASE_VERSION" "$package_source")
      IFS=$'\t' read -r package_tag package_commit <<< "$resolution"
      git -c advice.detachedHead=false clone --quiet --branch "$package_tag" -- "$package_source" "$package_stage"
      installed_commit=$(git -C "$package_stage" rev-parse --verify 'HEAD^{commit}')
      if [[ "$installed_commit" != "$package_commit" ]]; then
        rm -rf -- "$package_stage"
        echo "bootstrap package tag changed while cloning: $package_id@$package_tag" >&2
        exit 1
      fi
      git -C "$package_stage" config --local the8020.requestedTag "$package_tag"
    else
      git clone --quiet -- "$package_source" "$package_stage"
    fi
    if [[ ! -f "$package_stage/package.toml" ]]; then
      rm -rf -- "$package_stage"
      echo "bootstrap package is missing package.toml: $package_id" >&2
      exit 1
    fi
    # Local workspace snapshots include current uncommitted development source.
    # Give that exact snapshot a real commit so the database never needs a
    # second, filesystem-only package identity and later activation works
    # through the same Git path as remote packages.
    if [[ ! -d "$package_stage/.git" ]]; then
      git -C "$package_stage" init --quiet --initial-branch=main
      git -C "$package_stage" add --all
      GIT_AUTHOR_DATE='2000-01-01T00:00:00Z' \
      GIT_COMMITTER_DATE='2000-01-01T00:00:00Z' \
        git -C "$package_stage" \
        -c user.name='80|20 Installer' \
        -c user.email='installer@the8020.local' \
        -c commit.gpgsign=false \
        commit --quiet --message='Bootstrap package snapshot'
    fi
    mv -- "$package_stage" "$destination"
  done < <(awk -F '"' '
    /^\[\[packages\]\]$/ { if (id != "") print id "\t" source; id=""; source=""; next }
    /^id[[:space:]]*=/ { id=$2; next }
    /^source[[:space:]]*=/ { source=$2; next }
    END { if (id != "") print id "\t" source }
  ' "$SOURCE_ROOT/defaults/bootstrap-packages.toml")
fi

# Runtime definitions and source are one platform-owned build-input tree.
# Replace it atomically so removed tracked files cannot survive indefinitely in
# an existing instance.
RUNTIME_DEFINITIONS_STAGE=$(mktemp -d "$NODE_RUNTIME/.definitions-install.XXXXXX")
RUNTIME_DEFINITIONS_PREVIOUS=""
cleanup_runtime_refresh() {
  if [[ -n "$RUNTIME_DEFINITIONS_STAGE" && -e "$RUNTIME_DEFINITIONS_STAGE" ]]; then
    rm -rf -- "$RUNTIME_DEFINITIONS_STAGE"
  fi
  if [[ -n "$RUNTIME_DEFINITIONS_PREVIOUS" && -e "$RUNTIME_DEFINITIONS_PREVIOUS" && ! -e "$RUNTIME_DEFINITIONS" ]]; then
    mv -- "$RUNTIME_DEFINITIONS_PREVIOUS" "$RUNTIME_DEFINITIONS"
  fi
}
trap cleanup_runtime_refresh EXIT
chmod 0755 "$RUNTIME_DEFINITIONS_STAGE"
while IFS= read -r -d '' directory; do
  relative=${directory#"$RUNTIME_SOURCE"/}
  [[ "$directory" == "$RUNTIME_SOURCE" ]] && relative=""
  install -d -m 0755 "$RUNTIME_DEFINITIONS_STAGE/$relative"
done < <(find "$RUNTIME_SOURCE" -type d -print0)
while IFS= read -r -d '' source; do
  relative=${source#"$RUNTIME_SOURCE"/}
  install -m 0644 "$source" "$RUNTIME_DEFINITIONS_STAGE/$relative"
done < <(find "$RUNTIME_SOURCE" -type f -print0)
if [[ -e "$RUNTIME_DEFINITIONS" ]]; then
  RUNTIME_DEFINITIONS_PREVIOUS="$NODE_RUNTIME/.definitions-previous.$$"
  if [[ -e "$RUNTIME_DEFINITIONS_PREVIOUS" ]]; then
    echo "runtime definition backup path already exists: $RUNTIME_DEFINITIONS_PREVIOUS" >&2
    exit 1
  fi
  mv -- "$RUNTIME_DEFINITIONS" "$RUNTIME_DEFINITIONS_PREVIOUS"
fi
if ! mv -- "$RUNTIME_DEFINITIONS_STAGE" "$RUNTIME_DEFINITIONS"; then
  exit 1
fi
RUNTIME_DEFINITIONS_STAGE=""
if [[ -n "$RUNTIME_DEFINITIONS_PREVIOUS" ]]; then
  rm -rf -- "$RUNTIME_DEFINITIONS_PREVIOUS"
  RUNTIME_DEFINITIONS_PREVIOUS=""
fi
trap - EXIT

if [[ ! -f "$RUNTIME_DEFINITIONS/versions.toml" || ! -f "$RUNTIME_DEFINITIONS/image/Containerfile" || ! -f "$RUNTIME_DEFINITIONS/image/deno.json" ]]; then
  echo "instance runtime definitions are incomplete: $RUNTIME_DEFINITIONS" >&2
  exit 1
fi

echo "platform build [3/5]: refreshing portable generic runtime image" >&2
bash "$RUNTIME_SOURCE/install-portable.sh" \
  "$SOURCE_ROOT" \
  "$NODE_RUNTIME/images/rootless" \
  "$RUNTIME_DEFINITIONS/versions.toml" \
  "$RUNTIME_DEFINITIONS/image/Containerfile" \
  "$RUNTIME_DEFINITIONS/image/deno.json" \
  "$NODE_RUNTIME" \
  "$NODE_KERNEL/bin/runsc"

echo "platform build [4/5]: refreshing portable development image" >&2
bash "$RUNTIME_SOURCE/development/install-portable.sh" \
  "$SOURCE_ROOT" \
  "$RUNTIME_DEFINITIONS/development" \
  "$NODE_RUNTIME/images/development" \
  "$RUNTIME_DEFINITIONS/versions.toml" \
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
REQUESTED_RUNTIME_MODE=${THE8020_SANDBOX_RUNTIME_MODE:-auto}
if [[ "$SETUP_RUNTIME_HOST" == true && "$REQUESTED_RUNTIME_MODE" != rootless ]] && full_runtime_host_available; then
  bash "$RUNTIME_SOURCE/install-host.sh" "$SOURCE_ROOT" "$RUNTIME_DEFINITIONS/versions.toml" "$NODE_RUNTIME"
  FULL_RUNTIME_HOST=true
elif [[ "$REQUESTED_RUNTIME_MODE" == full ]]; then
  echo "full sandbox mode was requested but SYS_ADMIN, NET_ADMIN, and writable cgroup v2 authority are unavailable" >&2
  exit 1
else
  echo "sandbox install mode: rootless gVisor (full host authority unavailable or skipped)" >&2
fi

if [[ "$FULL_RUNTIME_HOST" == true ]]; then
  INSTANCE_UUID=$(sed -nE 's/^id[[:space:]]*=[[:space:]]*"([^"]+)"$/\1/p' "$KERNEL_CONFIG" | head -n 1)
  if [[ -z "$INSTANCE_UUID" ]]; then
    echo "node identity is missing" >&2
    exit 1
  fi
  bash "$RUNTIME_SOURCE/build-image.sh" \
    "$SOURCE_ROOT" "$RUNTIME_DEFINITIONS/image" "$NODE_RUNTIME/images/full" \
    "$RUNTIME_DEFINITIONS/versions.toml" "$NODE_RUNTIME" "$INSTANCE_UUID"
  bash "$RUNTIME_SOURCE/development/build-image.sh" \
    "$SOURCE_ROOT" "$RUNTIME_DEFINITIONS/development" "$NODE_RUNTIME/images/development" \
    "$RUNTIME_DEFINITIONS/versions.toml" "$NODE_RUNTIME"
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
  sh -n "$SOURCE_ROOT/defaults/scripts/activate"
  bash -n "$SOURCE_ROOT/defaults/scripts/install-codex.sh"
  bash -n "$SOURCE_ROOT/defaults/scripts/install-claude.sh"
  bash -n "$SOURCE_ROOT/release-tag.sh"
  bash -n "$SOURCE_ROOT/release-tag_test.sh"
  "$SOURCE_ROOT/release-tag_test.sh"
  "$DENO_CMD" fmt --check "$SOURCE_ROOT/defaults/scripts/activate.ts"
  "$DENO_CMD" lint "$SOURCE_ROOT/defaults/scripts/activate.ts"
  "$DENO_CMD" check "$SOURCE_ROOT/defaults/scripts/activate.ts"
else
  echo "platform verification gates skipped; required binaries and images are current" >&2
fi
