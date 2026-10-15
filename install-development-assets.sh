#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
exec bash "$SOURCE_ROOT/defaults/kernel-scripts/refresh-development-scripts.sh" "${1:?usage: install-development-assets.sh <instance-root>}" "$SOURCE_ROOT/defaults/scripts"
