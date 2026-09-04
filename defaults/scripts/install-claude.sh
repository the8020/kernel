#!/usr/bin/env bash
set -euo pipefail

printf '\nInstalling the latest Claude Code...\n\n'
curl -fsSL https://claude.ai/install.sh | bash

claude_config_dir=${CLAUDE_CONFIG_DIR:-"$HOME/.claude"}
claude_settings="$claude_config_dir/settings.json"
mkdir -p "$claude_config_dir"
claude_settings_temp=$(mktemp "$claude_config_dir/.settings.json.XXXXXX")
cleanup() {
  rm -f -- "$claude_settings_temp"
}
trap cleanup EXIT

CLAUDE_SETTINGS_PATH="$claude_settings" deno eval --no-config '
    const path = Deno.env.get("CLAUDE_SETTINGS_PATH");
    let settings = {};
    try {
      settings = JSON.parse(await Deno.readTextFile(path));
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound)) throw error;
    }
    if (settings === null || Array.isArray(settings) || typeof settings !== "object") {
      throw new Error("Claude settings must be a JSON object");
    }
    const permissions = settings.permissions ?? {};
    if (permissions === null || Array.isArray(permissions) || typeof permissions !== "object") {
      throw new Error("Claude permissions settings must be a JSON object");
    }
    settings.permissions = { ...permissions, defaultMode: "bypassPermissions" };
    settings.skipDangerousModePermissionPrompt = true;
    console.log(JSON.stringify(settings, null, 2));
  ' >"$claude_settings_temp"
chmod 0600 "$claude_settings_temp"
mv -f -- "$claude_settings_temp" "$claude_settings"
trap - EXIT

PATH="$HOME/.local/bin:$PATH" claude --version
printf '\n👍 Claude Code installed in YOLO mode.\n\nRun it with:\n  claude\n\n'
