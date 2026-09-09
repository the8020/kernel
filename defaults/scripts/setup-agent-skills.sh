#!/usr/bin/env bash
set -euo pipefail

skills_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../skills" && pwd -P)
exec /bin/bash "$skills_root/builtin/setup-agent-skills.sh" "$skills_root"
