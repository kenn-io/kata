#!/usr/bin/env bash
# Populate docs/assets/screenshots from the validated docs-assets branch.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
assets_branch="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
target="$docs_root/assets/screenshots"

# shellcheck source-path=SCRIPTDIR
# shellcheck source=assets.sh
. "$script_dir/assets.sh"

if kata_docs_validate_assets "$target" >/dev/null 2>&1; then
  exit 0
fi

if ! git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf 'docs screenshots not hydrated: no git worktree found and expected screenshots are missing\n' >&2
  exit 1
fi

if git -C "$repo_root" rev-parse --verify --quiet "refs/heads/$assets_branch" >/dev/null; then
  asset_ref="refs/heads/$assets_branch"
else
  if ! git -C "$repo_root" fetch --force --depth=1 origin \
    "+refs/heads/$assets_branch:refs/remotes/origin/$assets_branch" >/dev/null; then
    printf 'docs screenshots not hydrated: failed to fetch origin/%s\n' "$assets_branch" >&2
    exit 1
  fi
  asset_ref="refs/remotes/origin/$assets_branch"
fi

if ! git -C "$repo_root" rev-parse --verify --quiet "$asset_ref" >/dev/null; then
  printf 'docs screenshots not hydrated: %s branch unavailable\n' "$assets_branch" >&2
  exit 1
fi

mkdir -p "$docs_root/assets"
tmp_root="$(mktemp -d "$docs_root/assets/.screenshots-hydrate.XXXXXX")"
staged="$tmp_root/screenshots"
cleanup() {
  rm -rf "$tmp_root"
}
trap cleanup EXIT
mkdir -p "$staged"
git -C "$repo_root" archive "$asset_ref" | tar -xf - -C "$staged"
if ! kata_docs_validate_assets "$staged"; then
  printf 'docs screenshots not hydrated: %s failed the expected asset contract\n' "$assets_branch" >&2
  exit 1
fi

rm -rf "$target"
mv "$staged" "$target"
