#!/usr/bin/env bash
set -euo pipefail
umask 077

# Deployment conversion stays with deploy; ordinary native instances need no conversion.
if ! command -v caddy >/dev/null || ! command -v https-entrypoint >/dev/null; then exit 0; fi
case $(uname -m) in
  x86_64) architecture=amd64; checksum=509a43618876c5da48eb8747d286428b0184489c74f83e97fbe06ae52cbd6a04 ;;
  aarch64|arm64) architecture=arm64; checksum=e2872dd0220184a2a84bb0acd0f4fda4ff39d4c94954880b30d4983bcb4f6b8b ;;
  *) echo 'Unsupported HTTPS update architecture.' >&2; exit 1 ;;
esac
work="$THE8020_INSTANCE_ROOT/node/kernel/update/native-https"
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
curl --fail --silent --show-error --location --max-time 120 \
  "https://github.com/the8020/deploy/releases/download/0.9.0/https-update-linux-$architecture.tar.gz" \
  -o "$temporary/update.tar.gz"
printf '%s  %s\n' "$checksum" "$temporary/update.tar.gz" | sha256sum --check --status
mkdir -p "$work"
tar -xzf "$temporary/update.tar.gz" -C "$work"
bash "$work/update.sh" offline
