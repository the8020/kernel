#!/usr/bin/env bash
set -euo pipefail

: "${CODEX_VERSION:?CODEX_VERSION is required}"
apt-get update -qq
apt-get install -qq -y --no-install-recommends \
  bash ca-certificates coreutils curl findutils git grep nano ncurses-base \
  ncurses-bin nodejs npm sed
npm install --loglevel=error --global "@openai/codex@$CODEX_VERSION"
TERM=xterm clear >/dev/null
TERM=xterm-256color clear >/dev/null
apt-get clean
rm -rf /var/lib/apt/lists/*
rm -rf /var/cache/apt/archives/partial
install -d -o root -g root -m 0755 /workspace /workspace/packages
