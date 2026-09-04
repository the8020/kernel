#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
TEMP_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEMP_ROOT"' EXIT

create_remote() {
  local name=$1
  shift
  local repository="$TEMP_ROOT/$name.git"
  local checkout="$TEMP_ROOT/$name"
  git init --quiet --bare "$repository"
  git init --quiet --initial-branch=main "$checkout"
  git -C "$checkout" config user.name '80|20 Release Test'
  git -C "$checkout" config user.email 'release-test@the8020.local'
  printf 'release test\n' > "$checkout/package.toml"
  git -C "$checkout" add package.toml
  git -C "$checkout" commit --quiet --message initial
  for tag in "$@"; do
    git -C "$checkout" tag "$tag"
  done
  git -C "$checkout" remote add origin "$repository"
  git -C "$checkout" push --quiet origin main --tags
  printf '%s\n' "$repository"
}

selected_tag() {
  local result
  result=$("$SOURCE_ROOT/release-tag.sh" "$@")
  printf '%s\n' "${result%%$'\t'*}"
}

complete=$(create_remote complete 0.1.1 0.1.9 0.1.55 0.2.0 0.2.3 0.3.0 1.0.0)
git -C "$TEMP_ROOT/complete" tag --delete 0.2.3 >/dev/null
git -C "$TEMP_ROOT/complete" tag --annotate 0.2.3 --message 'annotated release'
git -C "$TEMP_ROOT/complete" push --quiet --force origin refs/tags/0.2.3
kernel_resolution=$("$SOURCE_ROOT/release-tag.sh" kernel 0.2 "$complete")
[[ "${kernel_resolution%%$'\t'*}" == 0.2.3 ]]
[[ "${kernel_resolution#*$'\t'}" == "$(git -C "$TEMP_ROOT/complete" rev-parse HEAD)" ]]
[[ "$(selected_tag package 0.2 "$complete")" == 0.2.3 ]]

fallback=$(create_remote fallback 0.1.1 0.1.55 0.3.0 0.8.0)
[[ "$(selected_tag package 0.2 "$fallback")" == 0.1.55 ]]
if "$SOURCE_ROOT/release-tag.sh" package 1.2 "$fallback" >/dev/null 2>&1; then
  echo "package resolution crossed a major-version boundary" >&2
  exit 1
fi
if "$SOURCE_ROOT/release-tag.sh" kernel 0.1.0 "$complete" >/dev/null 2>&1; then
  echo "release resolution accepted a three-part input version" >&2
  exit 1
fi

echo "release tag tests passed"
