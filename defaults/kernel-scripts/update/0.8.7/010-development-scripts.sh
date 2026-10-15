#!/usr/bin/env bash
set -euo pipefail

# Older updaters must refresh helpers even if the operator already repaired the symlink.
bash "$THE8020_UPDATE_RELEASE/scripts/refresh-development-scripts.sh" \
  "$THE8020_INSTANCE_ROOT" "$THE8020_UPDATE_RELEASE/development-scripts"
