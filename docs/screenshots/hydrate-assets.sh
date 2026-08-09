#!/usr/bin/env bash
# Populate docs/assets/screenshots from the validated docs-assets branch.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
assets_branch="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
pinned_commit="${KATA_DOCS_ASSETS_COMMIT:-}"
target="$docs_root/assets/screenshots"
force=false
case "${1:-}" in
  "")
    ;;
  --force)
    force=true
    ;;
  -h|--help)
    printf 'usage: %s [--force]\n' "${0##*/}"
    exit 0
    ;;
  *)
    printf 'usage: %s [--force]\n' "${0##*/}" >&2
    exit 2
    ;;
esac

# shellcheck source-path=SCRIPTDIR
# shellcheck source=assets.sh
. "$script_dir/assets.sh"

if [[ "$force" == false && -z "$pinned_commit" ]] && kata_docs_validate_assets "$target" >/dev/null 2>&1; then
  exit 0
fi

if ! git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf 'docs screenshots not hydrated: no git worktree found and expected screenshots are missing\n' >&2
  exit 1
fi

fetch_remote_assets() {
  local remote_ref="refs/remotes/origin/$assets_branch"
  if ! git -C "$repo_root" fetch --force --depth=1 origin \
    "+refs/heads/$assets_branch:$remote_ref" >/dev/null; then
    printf 'docs screenshots not hydrated: failed to fetch origin/%s\n' "$assets_branch" >&2
    exit 1
  fi
  asset_ref="$(git -C "$repo_root" rev-parse --verify "$remote_ref^{commit}")"
}

if [[ -n "$pinned_commit" ]]; then
  if [[ ! "$pinned_commit" =~ ^[0-9a-f]{40}$ ]] ||
    ! git -C "$repo_root" rev-parse --verify --quiet "$pinned_commit^{commit}" >/dev/null; then
    printf 'docs screenshots not hydrated: KATA_DOCS_ASSETS_COMMIT must name an available full commit ID\n' >&2
    exit 1
  fi
  asset_ref="$pinned_commit"
elif [[ "$force" == true ]]; then
  fetch_remote_assets
elif git -C "$repo_root" rev-parse --verify --quiet "refs/heads/$assets_branch" >/dev/null; then
  asset_ref="refs/heads/$assets_branch"
else
  fetch_remote_assets
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
