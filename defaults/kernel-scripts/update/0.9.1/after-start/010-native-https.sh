#!/usr/bin/env bash
set -euo pipefail

work="$THE8020_INSTANCE_ROOT/node/kernel/update/native-https"
if [[ -f "$work/update.sh" ]]; then bash "$work/update.sh" online; fi
