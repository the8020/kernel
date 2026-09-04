#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
  echo "usage: ./release-tag.sh <kernel|package> <major.minor> <git-source>" >&2
  exit 2
fi

kind=$1
version=$2
source=$3

if [[ "$kind" != kernel && "$kind" != package ]]; then
  echo "release tag kind must be kernel or package" >&2
  exit 2
fi
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "version must contain exactly major.minor without leading zeroes" >&2
  exit 2
fi

major=${version%%.*}
release_minor=${version#*.}

decimal_greater() {
  local left=$1 right=$2
  if (( ${#left} != ${#right} )); then
    (( ${#left} > ${#right} ))
    return
  fi
  [[ "$left" > "$right" ]]
}

if ! references=$(git ls-remote --tags "$source" "refs/tags/$major.*"); then
  echo "cannot list release tags for $source" >&2
  exit 1
fi

best_tag=""
best_minor=""
best_patch=""
while read -r _ reference; do
  [[ "$reference" == refs/tags/* ]] || continue
  tag=${reference#refs/tags/}
  if [[ ! "$tag" =~ ^${major}\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    continue
  fi
  minor=${BASH_REMATCH[1]}
  patch=${BASH_REMATCH[2]}
  if [[ "$kind" == kernel ]]; then
    [[ "$minor" == "$release_minor" ]] || continue
  elif decimal_greater "$minor" "$release_minor"; then
    continue
  fi
  if [[ -z "$best_tag" ]] || decimal_greater "$minor" "$best_minor" || {
    [[ "$minor" == "$best_minor" ]] && decimal_greater "$patch" "$best_patch"
  }; then
    best_tag=$tag
    best_minor=$minor
    best_patch=$patch
  fi
done <<< "$references"

if [[ -z "$best_tag" ]]; then
  if [[ "$kind" == kernel ]]; then
    echo "no kernel tag matches release line $version in $source" >&2
  else
    echo "no package tag is compatible with release line $version in $source" >&2
  fi
  exit 1
fi

tag_object=""
peeled_object=""
while read -r object reference; do
  case "$reference" in
    "refs/tags/$best_tag") tag_object=$object ;;
    "refs/tags/$best_tag^{}") peeled_object=$object ;;
  esac
done <<< "$references"
commit=${peeled_object:-$tag_object}
if [[ ! "$commit" =~ ^[0-9a-fA-F]{40,64}$ ]]; then
  echo "release tag $best_tag does not resolve to a Git object" >&2
  exit 1
fi

printf '%s\t%s\n' "$best_tag" "${commit,,}"
