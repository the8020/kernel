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
useradd --create-home --home-dir /home/developer --shell /bin/bash developer
mkdir -p /workspace/packages
chown developer:developer /home/developer /workspace /workspace/packages
chmod 0755 /home/developer
