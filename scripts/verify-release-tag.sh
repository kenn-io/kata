#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s <vMAJOR.MINOR.PATCH>\n' "$0" >&2
}

tag_name="${1:-${INPUT_TAG:-}}"
if [[ -z "$tag_name" ]]; then
  usage
  exit 2
fi

if [[ ! "$tag_name" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Invalid release tag: %s\n' "$tag_name" >&2
  printf 'Expected format: vMAJOR.MINOR.PATCH\n' >&2
  exit 2
fi

git fetch --force --tags origin \
  '+refs/heads/main:refs/remotes/origin/main' >/dev/null 2>&1

if ! tag_sha="$(git rev-parse -q --verify "${tag_name}^{commit}")"; then
  printf 'Release tag %s does not resolve to a commit\n' "$tag_name" >&2
  exit 1
fi

expected_sha="${EXPECTED_TAG_SHA:-}"
if [[ -n "$expected_sha" && "$tag_sha" != "$expected_sha" ]]; then
  printf 'Release tag %s commit %s does not match completed workflow SHA %s\n' \
    "$tag_name" "$tag_sha" "$expected_sha" >&2
  exit 1
fi

if ! git merge-base --is-ancestor "$tag_sha" origin/main; then
  printf 'Release tag %s commit %s is not contained in origin/main\n' \
    "$tag_name" "$tag_sha" >&2
  exit 1
fi

version="${tag_name#v}"
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    printf 'tag_name=%s\n' "$tag_name"
    printf 'version=%s\n' "$version"
    printf 'tag_sha=%s\n' "$tag_sha"
  } >>"$GITHUB_OUTPUT"
fi

printf 'Release tag %s verified at %s\n' "$tag_name" "$tag_sha"
