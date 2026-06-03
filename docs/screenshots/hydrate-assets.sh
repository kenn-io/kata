#!/usr/bin/env bash
# Populate docs/assets/screenshots from the docs-assets branch for production.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
assets_branch="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
target="$docs_root/assets/screenshots"
expected_assets=(
  "tui/hero.svg"
  "federation-tui/list.svg"
  "federation-tui/select-hub.svg"
  "federation-tui/select-hub-project.svg"
  "federation-tui/preview.svg"
  "federation-tui/result.svg"
)

has_expected_assets() {
  local asset
  for asset in "${expected_assets[@]}"; do
    [[ -f "$target/$asset" ]] || return 1
  done
}

if has_expected_assets; then
  exit 0
fi

if ! git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf 'docs screenshots not hydrated: no git worktree found and expected screenshots are missing\n' >&2
  exit 1
fi

if ! git -C "$repo_root" rev-parse --verify --quiet "$assets_branch" >/dev/null; then
  git -C "$repo_root" fetch --depth=1 origin \
    "$assets_branch:refs/remotes/origin/$assets_branch" >/dev/null 2>&1 || true
fi

asset_ref="$assets_branch"
if ! git -C "$repo_root" rev-parse --verify --quiet "$asset_ref" >/dev/null; then
  asset_ref="origin/$assets_branch"
fi
if ! git -C "$repo_root" rev-parse --verify --quiet "$asset_ref" >/dev/null; then
  printf 'docs screenshots not hydrated: %s branch unavailable\n' "$assets_branch" >&2
  exit 1
fi

rm -rf "$target"
mkdir -p "$target"
git -C "$repo_root" archive "$asset_ref" | tar -xf - -C "$target"
if ! has_expected_assets; then
  printf 'docs screenshots not hydrated: %s is missing expected screenshots\n' "$assets_branch" >&2
  exit 1
fi
