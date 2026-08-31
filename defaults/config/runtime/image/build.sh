#!/usr/bin/env bash
set -euo pipefail

apt-get update -qq
apt-get install -qq -y --no-install-recommends bash ncurses-base ncurses-bin
TERM=xterm clear >/dev/null
TERM=xterm-256color clear >/dev/null
apt-get clean
rm -rf /var/lib/apt/lists/*
mkdir -p /opt/runtime /artifacts /runtime-cache /tmp/runtime
chown -R deno:deno /runtime-cache /tmp/runtime
chmod 0755 /opt/runtime /artifacts
chmod 0700 /runtime-cache /tmp/runtime
