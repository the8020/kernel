#!/usr/bin/env bash
set -euo pipefail

# Mounted bootstrap also reaches retained system roots after a platform update.
/workspace/scripts/setup-agent-skills.sh
if [[ -f /workspace/skills/builtin/the8020-dev-uui-control/scripts/uui.ts ]]; then
  /workspace/scripts/uui auth >/dev/null || printf '%s\n' 'UUI sign-in is unavailable; uui will retry when used.' >&2
fi
exec /bin/bash /opt/development/sandbox.sh
