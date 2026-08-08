#!/usr/bin/env bash
# Generate or import docs screenshots and update the single-commit asset branch.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
assets_branch="${KATA_DOCS_ASSETS_BRANCH:-docs-assets}"
source_dir=""
generate=true
push=false

# shellcheck source-path=SCRIPTDIR
# shellcheck source=assets.sh
. "$script_dir/assets.sh"

usage() {
  cat <<EOF
Usage: $(basename "$0") [--source DIR] [--skip-generate] [--push]

Update the local $assets_branch branch to a single orphan commit containing
the exact generated documentation screenshot contract. By default the complete
asset set is regenerated first. Pass --source DIR to publish an existing set.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { printf 'ERROR: --source requires a directory\n' >&2; exit 2; }
      source_dir="$2"
      generate=false
      shift 2
      ;;
    --skip-generate)
      generate=false
      shift
      ;;
    --push)
      push=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if ! git check-ref-format --branch "$assets_branch" >/dev/null 2>&1; then
  printf 'invalid docs assets branch name: %s\n' "$assets_branch" >&2
  exit 1
fi

tmp_root="$(mktemp -d)"
asset_repo="$tmp_root/assets-repo"
generated_dir="$tmp_root/generated"
cleanup() {
  rm -rf "$tmp_root"
}
trap cleanup EXIT

if [[ "$generate" == true ]]; then
  "$script_dir/generate.sh" --out "$generated_dir"
  source_dir="$generated_dir"
elif [[ -z "$source_dir" ]]; then
  source_dir="$repo_root/docs/assets/screenshots"
fi

source_dir="$(cd "$source_dir" 2>/dev/null && pwd)" || {
  printf 'docs asset source does not exist: %s\n' "$source_dir" >&2
  exit 1
}
kata_docs_validate_assets "$source_dir"

mkdir -p "$asset_repo"
for asset in "${kata_docs_expected_assets[@]}"; do
  mkdir -p "$asset_repo/$(dirname "$asset")"
  cp "$source_dir/$asset" "$asset_repo/$asset"
done

git -C "$asset_repo" init --quiet
git -C "$asset_repo" add .
git -C "$asset_repo" \
  -c user.name="${GIT_AUTHOR_NAME:-kata docs bot}" \
  -c user.email="${GIT_AUTHOR_EMAIL:-docs-bot@example.invalid}" \
  commit -m "docs assets" >/dev/null
asset_commit="$(git -C "$asset_repo" rev-parse HEAD)"
git -C "$asset_repo" update-ref refs/heads/assets "$asset_commit"
git -C "$repo_root" fetch "$asset_repo" \
  "+refs/heads/assets:refs/heads/$assets_branch" >/dev/null

printf 'Updated %s -> %s\n' "$assets_branch" "$asset_commit"

if [[ "$push" == true ]]; then
  git -C "$repo_root" push --force origin "$assets_branch"
fi
