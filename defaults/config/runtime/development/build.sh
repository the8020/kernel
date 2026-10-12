#!/usr/bin/env bash
set -euo pipefail

apt-get update -qq
apt-get install -qq -y --no-install-recommends \
  bash ca-certificates coreutils curl findutils git grep nano ncurses-base \
  ncurses-bin procps sed
# A Docker/BuildKit outer build installs through a plain chroot without /proc,
# where procps cannot run; the portable installer still requires the executable.
if [[ -r /proc/self/stat ]]; then
  ps --version >/dev/null
else
  test -x /usr/bin/ps
fi
TERM=xterm clear >/dev/null
TERM=xterm-256color clear >/dev/null
apt-get clean
rm -rf /var/lib/apt/lists/*
rm -rf /var/cache/apt/archives/partial
install -d -o root -g root -m 0755 /workspace /workspace/packages
