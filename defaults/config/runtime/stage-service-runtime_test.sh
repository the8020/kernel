#!/usr/bin/env bash
set -euo pipefail

STAGER="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/stage-service-runtime.sh"
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT
SOURCE="$TEST_ROOT/source"
DENO="$SOURCE/defaults/config/runtime/deno"
mkdir -p "$DENO/http" "$DENO/examples" "$DENO/test"
printf '{}\n' > "$DENO/deno.json"
printf '{}\n' > "$DENO/deno.lock"
printf 'export {};\n' > "$DENO/http/mod.ts"
printf 'export {};\n' > "$DENO/http/the8020_http.d.ts"
touch "$DENO/AGENTS.md" "$DENO/http/http_test.ts" "$DENO/examples/demo.ts" "$DENO/test/helper.ts"

before=$(bash "$STAGER" "$SOURCE" --sources | xargs -0 sha256sum | sha256sum)
mkdir -p "$DENO/new-module/nested"
printf 'export const value = 1;\n' > "$DENO/new-module/nested/codec.ts"
after=$(bash "$STAGER" "$SOURCE" --sources | xargs -0 sha256sum | sha256sum)
[[ "$before" != "$after" ]]
bash "$STAGER" "$SOURCE" "$TEST_ROOT/image"
cmp "$DENO/new-module/nested/codec.ts" "$TEST_ROOT/image/new-module/nested/codec.ts"
cmp "$DENO/http/mod.ts" "$TEST_ROOT/image/http-source/mod.ts"
cmp "$DENO/http/the8020_http.d.ts" "$TEST_ROOT/image/http-source/the8020_http.d.ts"
[[ $(stat -c %a "$TEST_ROOT/image/new-module/nested/codec.ts") == 444 ]]
[[ $(find "$TEST_ROOT/image" -type f | wc -l) == 5 ]]
[[ ! -e "$TEST_ROOT/image/test" && ! -e "$TEST_ROOT/image/examples" ]]
echo 'Runtime staging checks passed: new nested modules are copied and hashed; tests, examples, and docs are excluded.'
