#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  printf 'usage: %s <bare-version> [extra changelog instructions]\n' "$0" >&2
  printf 'example: %s 0.5.0\n' "$0" >&2
}

version="${1:-}"
extra_instructions="${2:-}"

if [[ -z "$version" ]]; then
  usage
  exit 2
fi

if [[ "$version" == v* ]]; then
  printf 'version must be a bare version like 0.5.0, not %s\n' "$version" >&2
  exit 2
fi

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'version must use X.Y.Z semver shape\n' >&2
  exit 2
fi

tag="v${version}"

if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
  printf 'tag %s already exists\n' "$tag" >&2
  exit 1
fi

git update-index -q --refresh
if [[ -n "$(git status --porcelain)" ]]; then
  printf 'worktree is dirty; commit or stash changes before releasing\n' >&2
  exit 1
fi

notes_file="$(mktemp)"
trap 'rm -f "$notes_file"' EXIT

"$repo_root/scripts/changelog.sh" "$version" "-" "$extra_instructions" >"$notes_file"

printf 'Release %s\n\n' "$tag"
cat "$notes_file"
printf '\n'

printf 'Create and push %s? [y/N] ' "$tag"
answer=""
read -r answer || true
if [[ "$answer" != "y" && "$answer" != "Y" && "$answer" != "yes" && "$answer" != "YES" ]]; then
  printf 'release aborted\n' >&2
  exit 1
fi

tag_message="$(mktemp)"
trap 'rm -f "$notes_file" "$tag_message"' EXIT
{
  printf 'Release %s\n\n' "$version"
  cat "$notes_file"
} >"$tag_message"

git tag -a "$tag" -F "$tag_message"
git push origin "$tag"

printf 'pushed %s\n' "$tag"
