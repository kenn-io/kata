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
changelog_agent="${CHANGELOG_AGENT:-codex}"

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

printf 'Preparing release %s\n' "$tag"
printf '%s\n' '- Checking that the tag does not already exist and the worktree is clean.'
case "$changelog_agent" in
  codex)
    printf '%s\n' '- Generating release notes with CHANGELOG_AGENT=codex; this calls codex exec on the git log.'
    ;;
  claude)
    printf '%s\n' '- Generating release notes with CHANGELOG_AGENT=claude; this calls claude --print on the git log.'
    ;;
  none)
    printf '%s\n' '- Generating release notes with CHANGELOG_AGENT=none; this uses the deterministic git-log fallback.'
    ;;
  *)
    printf '%s\n' "- Generating release notes with CHANGELOG_AGENT=${changelog_agent}; scripts/changelog.sh will validate this value."
    ;;
esac
printf '%s\n\n' '- Showing the release notes preview before creating or pushing any tag.'

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

printf 'Creating annotated tag %s...\n' "$tag"
git tag -a "$tag" -F "$tag_message"
printf 'Pushing %s to origin...\n' "$tag"
git push origin "$tag"

printf 'pushed %s\n' "$tag"
