#!/usr/bin/env bash
set -euo pipefail
SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -rf -- "$FIXTURE_ROOT"' EXIT
mkdir -p "$FIXTURE_ROOT/tools" "$FIXTURE_ROOT/instance"
ln -s "$(command -v dirname)" "$FIXTURE_ROOT/tools/dirname"
printf '#!/bin/bash\nexit 0\n' > "$FIXTURE_ROOT/tools/git"
printf '#!/bin/bash\necho Linux\n' > "$FIXTURE_ROOT/tools/uname"
cat > "$FIXTURE_ROOT/tools/apt-get" <<'SCRIPT'
#!/bin/bash
printf '%s\n' "$*" >> "$FIXTURE_ROOT/apt-calls"
if [[ "$1" == install ]]; then exit 42; fi
SCRIPT
chmod +x "$FIXTURE_ROOT/tools/"{git,uname,apt-get}
export FIXTURE_ROOT
result=0
PATH="$FIXTURE_ROOT/tools" /bin/bash "$SOURCE_ROOT/install.sh" \
  --instance-root="$FIXTURE_ROOT/instance" --skip-runtime-host > "$FIXTURE_ROOT/output" 2>&1 || result=$?
if [[ $EUID == 0 ]]; then
  [[ "$result" == 42 ]]
  [[ $(cat "$FIXTURE_ROOT/apt-calls") == $'update\ninstall --yes --no-install-recommends bzip2' ]]
  rm "$FIXTURE_ROOT/tools/apt-get"
  result=0
  PATH="$FIXTURE_ROOT/tools" /bin/bash "$SOURCE_ROOT/install.sh" \
    --instance-root="$FIXTURE_ROOT/instance" --skip-runtime-host > "$FIXTURE_ROOT/output" 2>&1 || result=$?
fi
[[ "$result" == 1 ]]
grep -Fq 'bzip2 is required' "$FIXTURE_ROOT/output"

# A host below x86-64-v2 is refused before any download, except while building
# a Docker image, whose entrypoint checks the host that runs it.
printf 'flags\t\t: fpu sse sse2 cx16 lahf_lm\n' > "$FIXTURE_ROOT/cpuinfo"
for outer in false true; do
  result=0
  THE8020_CPU_ARCHITECTURE=x86_64 THE8020_CPUINFO="$FIXTURE_ROOT/cpuinfo" THE8020_OUTER_CONTAINER_BUILD=$outer \
    PATH="$FIXTURE_ROOT/tools" /bin/bash "$SOURCE_ROOT/install.sh" \
    --instance-root="$FIXTURE_ROOT/instance" --skip-runtime-host > "$FIXTURE_ROOT/output" 2>&1 || result=$?
  [[ "$result" == 1 ]]
  if [[ "$outer" == false ]]; then
    grep -Fq 'does not provide x86-64-v2' "$FIXTURE_ROOT/output"
  else
    ! grep -Fq 'x86-64-v2' "$FIXTURE_ROOT/output"
  fi
done
echo 'Installer dependency checks passed'
