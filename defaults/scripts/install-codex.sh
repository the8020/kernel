#!/usr/bin/env bash
set -euo pipefail

printf '\nInstalling the latest OpenAI Codex...\n\n'
curl -fsSL https://chatgpt.com/codex/install.sh |
  CODEX_RELEASE=latest CODEX_INSTALL_DIR="$HOME/.local/bin" CODEX_NON_INTERACTIVE=1 sh

codex_config_dir=${CODEX_HOME:-"$HOME/.codex"}
codex_config="$codex_config_dir/config.toml"
mkdir -p "$codex_config_dir"
codex_config_temp=$(mktemp "$codex_config_dir/.config.toml.XXXXXX")
cleanup() {
  rm -f -- "$codex_config_temp"
}
trap cleanup EXIT

{
  printf 'approval_policy = "never"\n'
  printf 'sandbox_mode = "danger-full-access"\n'
  if [[ -f "$codex_config" ]]; then
    awk '
      /^[[:space:]]*\[/ { in_table = 1 }
      !in_table && /^[[:space:]]*(approval_policy|sandbox_mode)[[:space:]]*=/ { next }
      { print }
    ' "$codex_config"
  fi
} >"$codex_config_temp"
chmod 0600 "$codex_config_temp"
mv -f -- "$codex_config_temp" "$codex_config"
trap - EXIT

PATH="$HOME/.local/bin:$PATH" codex --version
printf '\n👍 Codex installed in YOLO mode.\n\nRun it with:\n  codex\n\n'
