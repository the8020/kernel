#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=${1:-}
MANIFEST=${2:-"$SOURCE_ROOT/config/runtime/versions.toml"}
RUNTIME_ROOT=${3:-"$SOURCE_ROOT/node/kernel/runtime"}
RUNTIME_SOURCE="$SOURCE_ROOT/defaults/config/runtime"
if [[ -z "$SOURCE_ROOT" || ! -f "$MANIFEST" ]]; then
  echo "usage: defaults/config/runtime/install-host.sh <source-root> [versions-file] [node-runtime-root]" >&2
  exit 2
fi
if [[ $(uname -s) != Linux ]]; then
  echo "Phase 1B runtime host setup supports Linux only" >&2
  exit 1
fi
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    echo "Phase 1B runtime host setup requires host changes; requesting sudo" >&2
    exec sudo -- "$0" "$SOURCE_ROOT" "$MANIFEST" "$RUNTIME_ROOT"
  fi
  echo "Phase 1B runtime host setup requires root and sudo is unavailable" >&2
  exit 1
fi

if [[ ! -r /etc/os-release ]]; then
  echo "cannot identify host distribution; automatic setup supports Debian and Ubuntu" >&2
  exit 1
fi
. /etc/os-release
case "${ID:-}" in
  debian|ubuntu) ;;
  *)
    echo "automatic runtime setup does not support ${PRETTY_NAME:-${ID:-unknown}}" >&2
    echo "required components: containerd >= 2.0.0, runsc, containerd-shim-runsc-v1, CNI bridge/loopback/host-local, iproute2, nftables, and BuildKit" >&2
    exit 1
    ;;
esac

if [[ ! -f /sys/fs/cgroup/cgroup.controllers ]] || [[ $(stat -fc %T /sys/fs/cgroup 2>/dev/null || true) != cgroup2fs ]]; then
  echo "cgroup v2 is not mounted at /sys/fs/cgroup" >&2
  exit 1
fi
for controller in cpu memory pids; do
  if ! tr ' ' '\n' < /sys/fs/cgroup/cgroup.controllers | grep -Fqx "$controller"; then
    echo "required cgroup v2 controller is unavailable: $controller" >&2
    exit 1
  fi
done
CGROUP_OPTIONS=$(awk '$2 == "/sys/fs/cgroup" { print $4; exit }' /proc/mounts)
if [[ ",$CGROUP_OPTIONS," != *,rw,* ]]; then
  echo "cgroup v2 filesystem is read-only or not delegated at /sys/fs/cgroup" >&2
  exit 1
fi
if [[ ! -e /proc/self/ns/net || ! -r /proc/self/ns/net || ! -r /proc/mounts || ! -r /sys ]]; then
  echo "/proc, /sys, or Linux network namespaces are unavailable" >&2
  exit 1
fi
AVAILABLE_BYTES=$(df --output=avail -B1 "$SOURCE_ROOT" | awk 'NR == 2 { print $1 }')
if [[ ! "$AVAILABLE_BYTES" =~ ^[0-9]+$ ]] || (( AVAILABLE_BYTES < 5368709120 )); then
  echo "runtime setup requires at least 5 GiB of free project-filesystem space" >&2
  exit 1
fi

missing_packages=()
command -v curl >/dev/null 2>&1 || missing_packages+=(curl)
command -v sha256sum >/dev/null 2>&1 || missing_packages+=(coreutils)
command -v sha512sum >/dev/null 2>&1 || missing_packages+=(coreutils)
command -v bzip2 >/dev/null 2>&1 || missing_packages+=(bzip2)
command -v findmnt >/dev/null 2>&1 || missing_packages+=(util-linux)
command -v ip >/dev/null 2>&1 || missing_packages+=(iproute2)
command -v nft >/dev/null 2>&1 || missing_packages+=(nftables)
if (( ${#missing_packages[@]} > 0 )); then
  mapfile -t missing_packages < <(printf '%s\n' "${missing_packages[@]}" | sort -u)
  echo "privileged change: installing host packages: ${missing_packages[*]}" >&2
  DEBIAN_FRONTEND=noninteractive apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing_packages[@]}"
fi


DOWNLOADS="$RUNTIME_ROOT/downloads"
TEMP_ROOT="$RUNTIME_ROOT/tmp"
mkdir -p "$DOWNLOADS" "$TEMP_ROOT" "$RUNTIME_ROOT/cni/bin" "$RUNTIME_ROOT/cni/net.d" "$RUNTIME_ROOT/buildkit"

version_at_least() {
  local actual=${1#v} minimum=${2#v}
  [[ "$(printf '%s\n%s\n' "$minimum" "$actual" | sort -V | head -n1)" == "$minimum" ]]
}

backup_target() {
  local target=$1
  [[ -e "$target" || -L "$target" ]] || return 0
  local relative=${target#/}
  mkdir -p "$BACKUP_ROOT/$(dirname "$relative")"
  cp -a -- "$target" "$BACKUP_ROOT/$relative"
  echo "privileged change backup: $target -> $BACKUP_ROOT/$relative" >&2
}

toml_value() {
  local section=$1 key=$2
  awk -v wanted_section="[$section]" -v wanted_key="$key" '
    /^\[/ { section=$0 }
    section == wanted_section && $1 == wanted_key && $2 == "=" {
      value=$0; sub(/^[^=]+=[[:space:]]*/, "", value); gsub(/^"|"$/, "", value); print value; exit
    }
  ' "$MANIFEST"
}

download() {
  local url=$1 destination=$2
  if [[ -f "$destination" ]]; then return; fi
  local partial="$destination.partial"
  curl --fail --location --retry 3 --output "$partial" "$url"
  mv -f -- "$partial" "$destination"
}

verify_sha256() { printf '%s  %s\n' "$1" "$2" | sha256sum --check --status; }
verify_sha512() { printf '%s  %s\n' "$1" "$2" | sha512sum --check --status; }

case $(uname -m) in
  x86_64|amd64)
    ARCHIVE_ARCH=amd64
    GVISOR_ARCH=x86_64
    CONTAINERD_SHA=$(toml_value containerd archive_sha256_amd64)
    GVISOR_SHA=$(toml_value gvisor archive_sha512_amd64)
    RUNSC_SHA=$(toml_value gvisor runsc_sha512_amd64)
    SHIM_SHA=$(toml_value gvisor shim_sha512_amd64)
    CNI_SHA=$(toml_value cni archive_sha256_amd64)
    BUILDKIT_SHA=$(toml_value buildkit archive_sha256_amd64)
    ;;
  aarch64|arm64)
    ARCHIVE_ARCH=arm64
    GVISOR_ARCH=aarch64
    CONTAINERD_SHA=$(toml_value containerd archive_sha256_arm64)
    GVISOR_SHA=$(toml_value gvisor archive_sha512_arm64)
    RUNSC_SHA=$(toml_value gvisor runsc_sha512_arm64)
    SHIM_SHA=$(toml_value gvisor shim_sha512_arm64)
    CNI_SHA=$(toml_value cni archive_sha256_arm64)
    BUILDKIT_SHA=$(toml_value buildkit archive_sha256_arm64)
    ;;
  *) echo "unsupported runtime architecture: $(uname -m)" >&2; exit 1 ;;
esac

CONTAINERD_VERSION=$(toml_value containerd recommended_version)
GVISOR_RELEASE=$(toml_value gvisor release)
CNI_VERSION=$(toml_value cni version)
BUILDKIT_VERSION=$(toml_value buildkit version)
CONTAINERD_MINIMUM=$(toml_value containerd minimum_version)
BACKUP_ROOT="$RUNTIME_ROOT/backups/$(date -u +%Y%m%dT%H%M%SZ)"

CONTAINERD_BIN=$(command -v containerd || true)
CTR_BIN=$(command -v ctr || true)
INSTALL_CONTAINERD=true
if [[ -n "$CONTAINERD_BIN" && -n "$CTR_BIN" ]]; then
  CURRENT_CONTAINERD=$($CONTAINERD_BIN --version 2>/dev/null | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' | head -n1 || true)
  if [[ -n "$CURRENT_CONTAINERD" ]] && version_at_least "$CURRENT_CONTAINERD" "$CONTAINERD_MINIMUM"; then
    INSTALL_CONTAINERD=false
    echo "preserving compatible containerd $CURRENT_CONTAINERD at $CONTAINERD_BIN" >&2
  fi
fi

if [[ "$INSTALL_CONTAINERD" == true ]]; then
  CONTAINERD_ARCHIVE="$DOWNLOADS/containerd-$CONTAINERD_VERSION-linux-$ARCHIVE_ARCH.tar.gz"
  download "https://github.com/containerd/containerd/releases/download/v$CONTAINERD_VERSION/containerd-$CONTAINERD_VERSION-linux-$ARCHIVE_ARCH.tar.gz" "$CONTAINERD_ARCHIVE"
  verify_sha256 "$CONTAINERD_SHA" "$CONTAINERD_ARCHIVE"
  CONTAINERD_EXTRACT=$(mktemp -d "$TEMP_ROOT/containerd.XXXXXX")
  tar -xzf "$CONTAINERD_ARCHIVE" -C "$CONTAINERD_EXTRACT"
  for source in "$CONTAINERD_EXTRACT"/bin/*; do
    target="/usr/local/bin/$(basename "$source")"
    backup_target "$target"
    echo "privileged change: installing $target" >&2
    install -m 0755 "$source" "$target"
  done
  rm -rf -- "$CONTAINERD_EXTRACT"
  CONTAINERD_BIN=/usr/local/bin/containerd
  CTR_BIN=/usr/local/bin/ctr
fi

INSTALL_GVISOR=true
if command -v runsc >/dev/null 2>&1 && command -v containerd-shim-runsc-v1 >/dev/null 2>&1; then
  CURRENT_GVISOR=$(runsc --version 2>/dev/null | grep -Eo 'release-[0-9]{8}\.[0-9]+' | head -n1 | sed 's/^release-//' || true)
  if [[ -n "$CURRENT_GVISOR" ]] && version_at_least "$CURRENT_GVISOR" "$GVISOR_RELEASE"; then
    INSTALL_GVISOR=false
    echo "preserving compatible gVisor $CURRENT_GVISOR" >&2
  fi
fi
if [[ "$INSTALL_GVISOR" == true ]]; then
  GVISOR_ARCHIVE="$DOWNLOADS/gvisor-$GVISOR_RELEASE-$GVISOR_ARCH.tar.bz2"
  download "https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$GVISOR_ARCH/gvisor.tar.bz2" "$GVISOR_ARCHIVE"
  verify_sha512 "$GVISOR_SHA" "$GVISOR_ARCHIVE"
  GVISOR_EXTRACT=$(mktemp -d "$TEMP_ROOT/gvisor.XXXXXX")
  trap 'rm -rf -- "$GVISOR_EXTRACT"' EXIT
  tar -xjf "$GVISOR_ARCHIVE" -C "$GVISOR_EXTRACT"
  verify_sha512 "$RUNSC_SHA" "$GVISOR_EXTRACT/runsc"
  verify_sha512 "$SHIM_SHA" "$GVISOR_EXTRACT/containerd-shim-runsc-v1"
  for target in /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1 /usr/local/bin/gvisor-bin; do backup_target "$target"; done
  echo "privileged change: installing pinned gVisor binaries under /usr/local/bin" >&2
  install -m 0755 "$GVISOR_EXTRACT/runsc" /usr/local/bin/runsc
  install -m 0755 "$GVISOR_EXTRACT/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1
  rm -rf -- /usr/local/bin/gvisor-bin
  install -d -m 0755 /usr/local/bin/gvisor-bin
  find "$GVISOR_EXTRACT/gvisor-bin" -maxdepth 1 -type f -exec install -m 0755 '{}' /usr/local/bin/gvisor-bin/ \;
fi

CNI_ARCHIVE="$DOWNLOADS/cni-plugins-linux-$ARCHIVE_ARCH-v$CNI_VERSION.tgz"
download "https://github.com/containernetworking/plugins/releases/download/v$CNI_VERSION/cni-plugins-linux-$ARCHIVE_ARCH-v$CNI_VERSION.tgz" "$CNI_ARCHIVE"
verify_sha256 "$CNI_SHA" "$CNI_ARCHIVE"
rm -rf -- "$RUNTIME_ROOT/cni/bin"
install -d -m 0755 "$RUNTIME_ROOT/cni/bin"
tar -xzf "$CNI_ARCHIVE" -C "$RUNTIME_ROOT/cni/bin"
install -m 0644 "$RUNTIME_SOURCE/cni/the8020.conflist" "$RUNTIME_ROOT/cni/net.d/the8020.conflist"
install -d -m 0755 /opt/cni/bin /etc/cni/net.d
for source in "$RUNTIME_ROOT"/cni/bin/*; do
  target="/opt/cni/bin/$(basename "$source")"
  backup_target "$target"
  install -m 0755 "$source" "$target"
done
backup_target /etc/cni/net.d/the8020.conflist
install -m 0644 "$RUNTIME_SOURCE/cni/the8020.conflist" /etc/cni/net.d/the8020.conflist

BUILDKIT_ARCHIVE="$DOWNLOADS/buildkit-v$BUILDKIT_VERSION.linux-$ARCHIVE_ARCH.tar.gz"
download "https://github.com/moby/buildkit/releases/download/v$BUILDKIT_VERSION/buildkit-v$BUILDKIT_VERSION.linux-$ARCHIVE_ARCH.tar.gz" "$BUILDKIT_ARCHIVE"
verify_sha256 "$BUILDKIT_SHA" "$BUILDKIT_ARCHIVE"
rm -rf -- "$RUNTIME_ROOT/buildkit/bin"
tar -xzf "$BUILDKIT_ARCHIVE" -C "$RUNTIME_ROOT/buildkit"
for name in buildctl buildkitd; do
  backup_target "/usr/local/bin/$name"
  install -m 0755 "$RUNTIME_ROOT/buildkit/bin/$name" "/usr/local/bin/$name"
done

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  if [[ "$INSTALL_CONTAINERD" == true ]]; then
    install -d -m 0755 /etc/systemd/system/containerd.service.d
    backup_target /etc/systemd/system/containerd.service.d/the8020.conf
    echo "privileged change: configuring systemd to run $CONTAINERD_BIN" >&2
    printf '%s\n' '[Service]' 'ExecStart=' "ExecStart=$CONTAINERD_BIN" > /etc/systemd/system/containerd.service.d/the8020.conf
    systemctl daemon-reload
  fi
  echo "privileged change: enabling and starting containerd" >&2
  systemctl enable --now containerd
  systemctl restart containerd
else
  if ! "$CTR_BIN" --address /run/containerd/containerd.sock version >/dev/null 2>&1; then
    install -d -m 0755 /run/containerd
    install -d -m 0700 "$RUNTIME_ROOT/containerd/root" "$RUNTIME_ROOT/containerd/state"
    echo "privileged change: starting repository-managed containerd daemon" >&2
    nohup "$CONTAINERD_BIN" --address /run/containerd/containerd.sock --root "$RUNTIME_ROOT/containerd/root" --state "$RUNTIME_ROOT/containerd/state" >"$RUNTIME_ROOT/containerd/containerd.log" 2>&1 &
    printf '%s\n' "$!" > "$RUNTIME_ROOT/containerd/containerd.pid"
  fi
fi

for _ in $(seq 1 100); do
  if "$CTR_BIN" --address /run/containerd/containerd.sock version >/dev/null 2>&1; then break; fi
  sleep 0.05
done
"$CTR_BIN" --address /run/containerd/containerd.sock version >/dev/null
runsc --version >/dev/null
command -v ip >/dev/null
command -v nft >/dev/null

if [[ -n ${GVISOR_EXTRACT:-} ]]; then
  rm -rf -- "$GVISOR_EXTRACT"
  trap - EXIT
fi
